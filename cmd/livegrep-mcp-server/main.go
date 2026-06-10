package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	pb "github.com/livegrep/livegrep/src/proto/go_proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	backendAddr = flag.String("backend", "localhost:9999", "codesearch gRPC address")
)

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

type GetReposArgs struct{}

type SearchResult struct {
	Repo            string   `json:"repo"`
	Path            string   `json:"path"`
	Line            int64    `json:"line"`
	Content         string   `json:"content"`
	MatchBounds     [2]int   `json:"match_bounds"`
	ContextBefore   []string `json:"context_before"`
	ContextAfter    []string `json:"context_after"`
}

type SearchResponse struct {
	Results []SearchResult            `json:"results"`
	Stats   map[string]interface{} `json:"stats"`
}

type FileResult struct {
	Repo  string `json:"repo"`
	Path  string `json:"path"`
}

type FileSearchResponse struct {
	Results []FileResult              `json:"results"`
	Stats   map[string]interface{} `json:"stats"`
}

type RepoInfo struct {
	Name string `json:"name"`
}

type RepoResponse struct {
	Repos []RepoInfo `json:"repos"`
}

// JSON-RPC 2.0 types
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
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

var client pb.CodeSearchClient

func sendResponse(w *bufio.Writer, id interface{}, result interface{}, errMsg *JSONRPCError) error {
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
		Error:   errMsg,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return err
	}
	return w.Flush()
}

func handleToolsList(w *bufio.Writer, id interface{}) error {
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

	return sendResponse(w, id, ToolsListResult{Tools: tools}, nil)
}

func handleToolCall(w *bufio.Writer, id interface{}, call ToolCallRequest) error {
	switch call.Name {
	case "code_search":
		return handleCodeSearch(w, id, call.Arguments)
	case "search_files":
		return handleSearchFiles(w, id, call.Arguments)
	case "get_repos":
		return handleGetRepos(w, id, call.Arguments)
	default:
		return sendResponse(w, id, nil, &JSONRPCError{
			Code:    -32601,
			Message: fmt.Sprintf("Method not found: %s", call.Name),
		})
	}
}

func handleInitialize(w *bufio.Writer, id interface{}) error {
	result := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "livegrep-mcp-server",
			"version": "1.0.0",
		},
	}
	return sendResponse(w, id, result, nil)
}

func handleCodeSearch(w *bufio.Writer, id interface{}, args map[string]interface{}) error {
	var searchArgs CodeSearchArgs
	if data, err := json.Marshal(args); err != nil {
		return sendResponse(w, id, nil, &JSONRPCError{Code: -32600, Message: "Invalid request"})
	} else if err := json.Unmarshal(data, &searchArgs); err != nil {
		return sendResponse(w, id, nil, &JSONRPCError{Code: -32600, Message: "Invalid arguments"})
	}

	if searchArgs.Query == "" {
		return sendResponse(w, id, nil, &JSONRPCError{Code: -32602, Message: "query parameter is required"})
	}

	log.Printf("[%v] Code search query=%q repo=%q file=%q case_sensitive=%v max_matches=%v", id, searchArgs.Query, searchArgs.Repo, searchArgs.File, searchArgs.CaseSensitive, searchArgs.MaxMatches)

	q := &pb.Query{
		Line:       searchArgs.Query,
		MaxMatches: 100,
	}

	if searchArgs.Repo != "" {
		q.Repo = searchArgs.Repo
	}

	if searchArgs.File != "" {
		q.File = []string{searchArgs.File}
	}

	q.FoldCase = !searchArgs.CaseSensitive

	if searchArgs.MaxMatches > 0 {
		q.MaxMatches = int32(searchArgs.MaxMatches)
	}

	if searchArgs.ContextLines > 0 {
		q.ContextLines = int32(searchArgs.ContextLines)
	} else {
		q.ContextLines = 2
	}

	searchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := client.Search(searchCtx, q)
	if err != nil {
		return sendResponse(w, id, nil, &JSONRPCError{
			Code:    -32603,
			Message: fmt.Sprintf("Internal error: %v", err),
		})
	}

	results := make([]SearchResult, 0)
	for _, r := range result.Results {
		results = append(results, SearchResult{
			Repo:          r.Tree,
			Path:          r.Path,
			Line:          r.LineNumber,
			Content:       r.Line,
			MatchBounds:   [2]int{int(r.Bounds.Left), int(r.Bounds.Right)},
			ContextBefore: r.ContextBefore,
			ContextAfter:  r.ContextAfter,
		})
	}

	response := SearchResponse{
		Results: results,
		Stats: map[string]interface{}{
			"total_matches": len(results),
			"search_time_ms": result.Stats.GetAnalyzeTime() + result.Stats.GetRe2Time(),
		},
	}

	log.Printf("[%v] Code search completed: %d matches", id, len(results))
	return sendResponse(w, id, response, nil)
}

func handleSearchFiles(w *bufio.Writer, id interface{}, args map[string]interface{}) error {
	var searchArgs SearchFileArgs
	if data, err := json.Marshal(args); err != nil {
		return sendResponse(w, id, nil, &JSONRPCError{Code: -32600, Message: "Invalid request"})
	} else if err := json.Unmarshal(data, &searchArgs); err != nil {
		return sendResponse(w, id, nil, &JSONRPCError{Code: -32600, Message: "Invalid arguments"})
	}

	if searchArgs.Pattern == "" {
		return sendResponse(w, id, nil, &JSONRPCError{Code: -32602, Message: "pattern parameter is required"})
	}

	log.Printf("[%v] File search pattern=%q repo=%q max_matches=%v", id, searchArgs.Pattern, searchArgs.Repo, searchArgs.MaxMatches)

	q := &pb.Query{
		Line:         searchArgs.Pattern,
		FilenameOnly: true,
		MaxMatches:   100,
	}

	if searchArgs.Repo != "" {
		q.Repo = searchArgs.Repo
	}

	if searchArgs.MaxMatches > 0 {
		q.MaxMatches = int32(searchArgs.MaxMatches)
	}

	searchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := client.Search(searchCtx, q)
	if err != nil {
		return sendResponse(w, id, nil, &JSONRPCError{
			Code:    -32603,
			Message: fmt.Sprintf("Internal error: %v", err),
		})
	}

	results := make([]FileResult, 0)
	for _, r := range result.FileResults {
		results = append(results, FileResult{
			Repo: r.Tree,
			Path: r.Path,
		})
	}

	response := FileSearchResponse{
		Results: results,
		Stats: map[string]interface{}{
			"total_matches": len(results),
		},
	}

	log.Printf("[%v] File search completed: %d matches", id, len(results))
	return sendResponse(w, id, response, nil)
}

func handleGetRepos(w *bufio.Writer, id interface{}, args map[string]interface{}) error {
	log.Printf("[%v] Getting available repositories", id)
	repoCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := client.Info(repoCtx, &pb.InfoRequest{})
	if err != nil {
		log.Printf("[%v] Failed to get repos: %v", id, err)
		return sendResponse(w, id, nil, &JSONRPCError{
			Code:    -32603,
			Message: fmt.Sprintf("Internal error: %v", err),
		})
	}

	repos := make([]RepoInfo, 0)
	for _, tree := range info.Trees {
		repos = append(repos, RepoInfo{Name: tree.Name})
	}

	log.Printf("[%v] Found %d repositories", id, len(repos))
	response := RepoResponse{Repos: repos}
	return sendResponse(w, id, response, nil)
}

func main() {
	flag.Parse()

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	log.Printf("Connecting to codesearch at %s", *backendAddr)
	conn, err := grpc.Dial(*backendAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to connect to codesearch: %v", err)
	}
	defer conn.Close()

	client = pb.NewCodeSearchClient(conn)
	log.Printf("Connected to codesearch backend")

	scanner := bufio.NewScanner(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)

	for scanner.Scan() {
		var req Request
		line := scanner.Bytes()

		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("Failed to parse request: %v", err)
			sendResponse(writer, nil, nil, &JSONRPCError{
				Code:    -32700,
				Message: "Parse error",
			})
			continue
		}

		log.Printf("[%v] Received %s request", req.ID, req.Method)

		if req.JSONRPC != "2.0" {
			log.Printf("Invalid JSON-RPC version: %s", req.JSONRPC)
			sendResponse(writer, req.ID, nil, &JSONRPCError{
				Code:    -32600,
				Message: "Invalid Request",
			})
			continue
		}

		switch req.Method {
		case "initialize":
			log.Printf("[%v] Handling initialize", req.ID)
			handleInitialize(writer, req.ID)
		case "tools/list":
			log.Printf("[%v] Listing available tools", req.ID)
			handleToolsList(writer, req.ID)
		case "tools/call":
			var call ToolCallRequest
			if err := json.Unmarshal(req.Params, &call); err != nil {
				log.Printf("[%v] Failed to parse tool call: %v", req.ID, err)
				sendResponse(writer, req.ID, nil, &JSONRPCError{
					Code:    -32600,
					Message: "Invalid arguments",
				})
			} else {
				log.Printf("[%v] Calling tool: %s with args: %v", req.ID, call.Name, call.Arguments)
				handleToolCall(writer, req.ID, call)
				log.Printf("[%v] Tool call completed", req.ID)
			}
		default:
			log.Printf("Unknown method: %s", req.Method)
			sendResponse(writer, req.ID, nil, &JSONRPCError{
				Code:    -32601,
				Message: "Method not found",
			})
		}
	}

	if err := scanner.Err(); err != nil {
		log.Fatalf("Scanner error: %v", err)
	}
}
