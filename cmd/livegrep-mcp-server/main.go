// Command livegrep-mcp-server exposes a livegrep codesearch backend over the
// Model Context Protocol (MCP) using newline-delimited JSON-RPC 2.0 on stdio.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	pb "github.com/livegrep/livegrep/src/proto/go_proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	protocolVersion = "2024-11-05"
	serverName      = "livegrep-mcp-server"
	serverVersion   = "1.0.0"

	defaultMaxMatches   = 100
	defaultContextLines = 2
	searchTimeout       = 30 * time.Second
	infoTimeout         = 10 * time.Second
)

var backendAddr = flag.String("backend", "localhost:9999", "codesearch gRPC address")

// ---------------------------------------------------------------------------
// Tool argument and result types
// ---------------------------------------------------------------------------

type CodeSearchArgs struct {
	Query         string  `json:"query"`
	Repo          string  `json:"repo"`
	File          string  `json:"file"`
	CaseSensitive bool    `json:"case_sensitive"`
	MaxMatches    float64 `json:"max_matches"`
	ContextLines  float64 `json:"context_lines"`
}

type SearchFileArgs struct {
	Pattern    string  `json:"pattern"`
	Repo       string  `json:"repo"`
	MaxMatches float64 `json:"max_matches"`
}

type SearchResult struct {
	Repo          string   `json:"repo"`
	Path          string   `json:"path"`
	Line          int64    `json:"line"`
	Content       string   `json:"content"`
	MatchBounds   [2]int   `json:"match_bounds"`
	ContextBefore []string `json:"context_before"`
	ContextAfter  []string `json:"context_after"`
}

type SearchResponse struct {
	Results []SearchResult         `json:"results"`
	Stats   map[string]interface{} `json:"stats"`
}

type FileResult struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
}

type FileSearchResponse struct {
	Results []FileResult           `json:"results"`
	Stats   map[string]interface{} `json:"stats"`
}

type RepoInfo struct {
	Name string `json:"name"`
}

type RepoResponse struct {
	Repos []RepoInfo `json:"repos"`
}

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 / MCP protocol types
// ---------------------------------------------------------------------------

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type Response struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      interface{}   `json:"id"`
	Result  interface{}   `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type ToolDescriptor struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

type ToolsListResult struct {
	Tools []ToolDescriptor `json:"tools"`
}

type ToolCallRequest struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// ToolContent and ToolCallResult model the MCP tools/call result envelope.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolCallResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// JSON-RPC standard error codes.
const (
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
)

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

// Server handles MCP requests, delegating searches to a codesearch backend.
// The backend is the generated pb.CodeSearchClient interface so it can be
// replaced with a fake in tests.
type Server struct {
	client pb.CodeSearchClient
}

func NewServer(client pb.CodeSearchClient) *Server {
	return &Server{client: client}
}

// Serve reads newline-delimited JSON-RPC requests from in and writes responses
// to out until in is exhausted (EOF) or a read/write error occurs.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	encoder := json.NewEncoder(out)

	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			if resp := s.process(ctx, trimmed); resp != nil {
				if encErr := encoder.Encode(resp); encErr != nil {
					return encErr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// process parses a single JSON-RPC message and returns the response to send,
// or nil if the message is a notification that requires no response.
func (s *Server) process(ctx context.Context, line []byte) *Response {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		log.Printf("parse error: %v", err)
		return errorResponse(nil, errParse, "Parse error")
	}
	return s.handleRequest(ctx, &req)
}

// handleRequest dispatches a parsed request. It returns nil for notifications
// (any method under "notifications/", or any request without an id), which per
// the JSON-RPC spec must not receive a response.
func (s *Server) handleRequest(ctx context.Context, req *Request) *Response {
	isNotification := req.ID == nil

	if req.JSONRPC != "2.0" {
		if isNotification {
			return nil
		}
		log.Printf("invalid JSON-RPC version: %q", req.JSONRPC)
		return errorResponse(req.ID, errInvalidRequest, "Invalid Request")
	}

	log.Printf("[%v] %s", req.ID, req.Method)

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.ID)
	case "notifications/initialized":
		return nil
	case "ping":
		return successResponse(req.ID, struct{}{})
	case "tools/list":
		return s.handleToolsList(req.ID)
	case "tools/call":
		var call ToolCallRequest
		if err := json.Unmarshal(req.Params, &call); err != nil {
			return errorResponse(req.ID, errInvalidRequest, "Invalid arguments")
		}
		return s.handleToolCall(ctx, req.ID, call)
	default:
		if isNotification {
			return nil
		}
		log.Printf("unknown method: %s", req.Method)
		return errorResponse(req.ID, errMethodNotFound, "Method not found")
	}
}

func (s *Server) handleInitialize(id interface{}) *Response {
	return successResponse(id, map[string]interface{}{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    serverName,
			"version": serverVersion,
		},
	})
}

func (s *Server) handleToolsList(id interface{}) *Response {
	tools := []ToolDescriptor{
		{
			Name:        "code_search",
			Description: "Search for code patterns using regex",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "Regex pattern to search for",
					},
					"repo": map[string]interface{}{
						"type":        "string",
						"description": "Filter by repository pattern (optional)",
					},
					"file": map[string]interface{}{
						"type":        "string",
						"description": "Filter by file path pattern (optional)",
					},
					"case_sensitive": map[string]interface{}{
						"type":        "boolean",
						"description": "Case-sensitive search (default false)",
					},
					"max_matches": map[string]interface{}{
						"type":        "number",
						"description": "Maximum number of matches to return (default 100)",
					},
					"context_lines": map[string]interface{}{
						"type":        "number",
						"description": "Number of context lines to include (default 2)",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "search_files",
			Description: "Search for files by name or path pattern",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pattern": map[string]interface{}{
						"type":        "string",
						"description": "File path regex pattern to search",
					},
					"repo": map[string]interface{}{
						"type":        "string",
						"description": "Filter by repository pattern (optional)",
					},
					"max_matches": map[string]interface{}{
						"type":        "number",
						"description": "Maximum number of matches to return (default 100)",
					},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "get_repos",
			Description: "List all indexed repositories",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
				"required":   []string{},
			},
		},
	}
	return successResponse(id, ToolsListResult{Tools: tools})
}

func (s *Server) handleToolCall(ctx context.Context, id interface{}, call ToolCallRequest) *Response {
	var (
		payload interface{}
		err     error
	)
	switch call.Name {
	case "code_search":
		payload, err = s.codeSearch(ctx, call.Arguments)
	case "search_files":
		payload, err = s.searchFiles(ctx, call.Arguments)
	case "get_repos":
		payload, err = s.getRepos(ctx)
	default:
		return errorResponse(id, errMethodNotFound, fmt.Sprintf("Unknown tool: %s", call.Name))
	}
	return toolResponse(id, payload, err)
}

// ---------------------------------------------------------------------------
// Tool implementations
// ---------------------------------------------------------------------------

func (s *Server) codeSearch(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	var a CodeSearchArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Query == "" {
		return nil, fmt.Errorf("query parameter is required")
	}

	log.Printf("code_search query=%q repo=%q file=%q case_sensitive=%v max_matches=%v",
		a.Query, a.Repo, a.File, a.CaseSensitive, a.MaxMatches)

	q := &pb.Query{
		Line:         a.Query,
		Repo:         a.Repo,
		FoldCase:     !a.CaseSensitive,
		MaxMatches:   defaultMaxMatches,
		ContextLines: defaultContextLines,
	}
	if a.File != "" {
		q.File = []string{a.File}
	}
	if a.MaxMatches > 0 {
		q.MaxMatches = int32(a.MaxMatches)
	}
	if a.ContextLines > 0 {
		q.ContextLines = int32(a.ContextLines)
	}

	searchCtx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	result, err := s.client.Search(searchCtx, q)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	results := make([]SearchResult, 0, len(result.Results))
	for _, r := range result.Results {
		var bounds [2]int
		if r.Bounds != nil {
			bounds = [2]int{int(r.Bounds.Left), int(r.Bounds.Right)}
		}
		results = append(results, SearchResult{
			Repo:          r.Tree,
			Path:          r.Path,
			Line:          r.LineNumber,
			Content:       r.Line,
			MatchBounds:   bounds,
			ContextBefore: r.ContextBefore,
			ContextAfter:  r.ContextAfter,
		})
	}

	log.Printf("code_search completed: %d matches", len(results))
	return SearchResponse{
		Results: results,
		Stats: map[string]interface{}{
			"total_matches":  len(results),
			"search_time_ms": result.Stats.GetAnalyzeTime() + result.Stats.GetRe2Time(),
		},
	}, nil
}

func (s *Server) searchFiles(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	var a SearchFileArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Pattern == "" {
		return nil, fmt.Errorf("pattern parameter is required")
	}

	log.Printf("search_files pattern=%q repo=%q max_matches=%v", a.Pattern, a.Repo, a.MaxMatches)

	q := &pb.Query{
		Line:         a.Pattern,
		Repo:         a.Repo,
		FilenameOnly: true,
		MaxMatches:   defaultMaxMatches,
	}
	if a.MaxMatches > 0 {
		q.MaxMatches = int32(a.MaxMatches)
	}

	searchCtx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	result, err := s.client.Search(searchCtx, q)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	results := make([]FileResult, 0, len(result.FileResults))
	for _, r := range result.FileResults {
		results = append(results, FileResult{Repo: r.Tree, Path: r.Path})
	}

	log.Printf("search_files completed: %d matches", len(results))
	return FileSearchResponse{
		Results: results,
		Stats: map[string]interface{}{
			"total_matches": len(results),
		},
	}, nil
}

func (s *Server) getRepos(ctx context.Context) (interface{}, error) {
	log.Printf("get_repos")

	infoCtx, cancel := context.WithTimeout(ctx, infoTimeout)
	defer cancel()

	info, err := s.client.Info(infoCtx, &pb.InfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("info failed: %w", err)
	}

	repos := make([]RepoInfo, 0, len(info.Trees))
	for _, tree := range info.Trees {
		repos = append(repos, RepoInfo{Name: tree.Name})
	}

	log.Printf("get_repos completed: %d repositories", len(repos))
	return RepoResponse{Repos: repos}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// decodeArgs converts the loosely-typed MCP argument map into a typed struct.
func decodeArgs(args map[string]interface{}, dst interface{}) error {
	data, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func successResponse(id interface{}, result interface{}) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResponse(id interface{}, code int, message string) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Error: &JSONRPCError{Code: code, Message: message}}
}

// toolResponse wraps a tool's payload (or error) in the MCP tools/call result
// envelope. Tool errors are reported as a successful JSON-RPC response with
// isError set, per the MCP specification.
func toolResponse(id interface{}, payload interface{}, err error) *Response {
	if err != nil {
		return successResponse(id, ToolCallResult{
			Content: []ToolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		})
	}
	data, marshalErr := json.MarshalIndent(payload, "", "  ")
	if marshalErr != nil {
		return successResponse(id, ToolCallResult{
			Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("failed to encode result: %v", marshalErr)}},
			IsError: true,
		})
	}
	return successResponse(id, ToolCallResult{
		Content: []ToolContent{{Type: "text", Text: string(data)}},
	})
}

func main() {
	flag.Parse()

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	log.Printf("connecting to codesearch at %s", *backendAddr)
	conn, err := grpc.NewClient(*backendAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("failed to create codesearch client: %v", err)
	}
	defer conn.Close()

	srv := NewServer(pb.NewCodeSearchClient(conn))
	log.Printf("serving MCP on stdio")

	if err := srv.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
