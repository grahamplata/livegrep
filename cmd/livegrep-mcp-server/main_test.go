package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	pb "github.com/livegrep/livegrep/src/proto/go_proto"
	"google.golang.org/grpc"
)

// fakeClient is a test double for pb.CodeSearchClient. Each method delegates to
// a function field so individual tests can control behavior.
type fakeClient struct {
	search func(*pb.Query) (*pb.CodeSearchResult, error)
	info   func(*pb.InfoRequest) (*pb.ServerInfo, error)
}

func (f *fakeClient) Search(_ context.Context, q *pb.Query, _ ...grpc.CallOption) (*pb.CodeSearchResult, error) {
	return f.search(q)
}

func (f *fakeClient) Info(_ context.Context, r *pb.InfoRequest, _ ...grpc.CallOption) (*pb.ServerInfo, error) {
	return f.info(r)
}

func (f *fakeClient) Reload(_ context.Context, _ *pb.Empty, _ ...grpc.CallOption) (*pb.Empty, error) {
	return &pb.Empty{}, nil
}

func newRequest(t *testing.T, id interface{}, method string, params interface{}) *Request {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		raw = data
	}
	return &Request{JSONRPC: "2.0", ID: id, Method: method, Params: raw}
}

func TestInitialize(t *testing.T) {
	s := NewServer(&fakeClient{})
	resp := s.handleRequest(context.Background(), newRequest(t, 1, "initialize", nil))
	if resp == nil || resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp)
	}
	result := resp.Result.(map[string]interface{})
	if result["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v, want %v", result["protocolVersion"], protocolVersion)
	}
}

func TestNotificationsGetNoResponse(t *testing.T) {
	s := NewServer(&fakeClient{})
	for _, method := range []string{"notifications/initialized", "notifications/cancelled"} {
		req := &Request{JSONRPC: "2.0", Method: method} // no id => notification
		if resp := s.handleRequest(context.Background(), req); resp != nil {
			t.Errorf("%s: got response %+v, want nil", method, resp)
		}
	}
}

func TestToolsList(t *testing.T) {
	s := NewServer(&fakeClient{})
	resp := s.handleRequest(context.Background(), newRequest(t, 1, "tools/list", nil))
	if resp == nil || resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp)
	}
	tools := resp.Result.(ToolsListResult).Tools
	want := map[string]bool{"code_search": true, "search_files": true, "get_repos": true}
	if len(tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(tools), len(want))
	}
	for _, tool := range tools {
		if !want[tool.Name] {
			t.Errorf("unexpected tool %q", tool.Name)
		}
	}
}

func TestUnknownMethod(t *testing.T) {
	s := NewServer(&fakeClient{})
	resp := s.handleRequest(context.Background(), newRequest(t, 1, "does/not/exist", nil))
	if resp == nil || resp.Error == nil {
		t.Fatalf("expected error response, got %+v", resp)
	}
	if resp.Error.Code != errMethodNotFound {
		t.Errorf("code = %d, want %d", resp.Error.Code, errMethodNotFound)
	}
}

func callTool(t *testing.T, s *Server, name string, args map[string]interface{}) *ToolCallResult {
	t.Helper()
	resp := s.handleRequest(context.Background(), newRequest(t, 1, "tools/call", ToolCallRequest{
		Name:      name,
		Arguments: args,
	}))
	if resp == nil || resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp)
	}
	res, ok := resp.Result.(ToolCallResult)
	if !ok {
		t.Fatalf("result is %T, want ToolCallResult", resp.Result)
	}
	return &res
}

func TestCodeSearchSuccess(t *testing.T) {
	var gotQuery *pb.Query
	s := NewServer(&fakeClient{
		search: func(q *pb.Query) (*pb.CodeSearchResult, error) {
			gotQuery = q
			return &pb.CodeSearchResult{
				Stats: &pb.SearchStats{Re2Time: 5, AnalyzeTime: 3},
				Results: []*pb.SearchResult{
					{Tree: "repo1", Path: "a.go", LineNumber: 10, Line: "hello", Bounds: &pb.Bounds{Left: 0, Right: 5}},
				},
			}, nil
		},
	})

	res := callTool(t, s, "code_search", map[string]interface{}{"query": "hello", "repo": "repo1"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", res.Content[0].Text)
	}

	// The query passed to the backend should carry our arguments and defaults.
	if gotQuery.Line != "hello" || gotQuery.Repo != "repo1" {
		t.Errorf("query = %+v, want Line=hello Repo=repo1", gotQuery)
	}
	if !gotQuery.FoldCase {
		t.Error("FoldCase should be true when case_sensitive is unset")
	}
	if gotQuery.ContextLines != defaultContextLines {
		t.Errorf("ContextLines = %d, want %d", gotQuery.ContextLines, defaultContextLines)
	}

	// The MCP envelope must contain one text content block with the JSON payload.
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want single text block", res.Content)
	}
	var payload SearchResponse
	if err := json.Unmarshal([]byte(res.Content[0].Text), &payload); err != nil {
		t.Fatalf("content is not valid JSON: %v", err)
	}
	if len(payload.Results) != 1 || payload.Results[0].Repo != "repo1" {
		t.Errorf("results = %+v", payload.Results)
	}
}

func TestCodeSearchMissingQuery(t *testing.T) {
	s := NewServer(&fakeClient{})
	res := callTool(t, s, "code_search", map[string]interface{}{})
	if !res.IsError {
		t.Fatal("expected isError result for missing query")
	}
	if !strings.Contains(res.Content[0].Text, "query parameter is required") {
		t.Errorf("unexpected error text: %s", res.Content[0].Text)
	}
}

func TestCodeSearchNilBounds(t *testing.T) {
	s := NewServer(&fakeClient{
		search: func(*pb.Query) (*pb.CodeSearchResult, error) {
			return &pb.CodeSearchResult{
				Stats:   &pb.SearchStats{},
				Results: []*pb.SearchResult{{Tree: "r", Path: "p", Line: "x"}}, // Bounds nil
			}, nil
		},
	})
	res := callTool(t, s, "code_search", map[string]interface{}{"query": "x"})
	if res.IsError {
		t.Fatalf("nil bounds should not error: %s", res.Content[0].Text)
	}
}

func TestSearchFiles(t *testing.T) {
	var gotQuery *pb.Query
	s := NewServer(&fakeClient{
		search: func(q *pb.Query) (*pb.CodeSearchResult, error) {
			gotQuery = q
			return &pb.CodeSearchResult{
				Stats:       &pb.SearchStats{},
				FileResults: []*pb.FileResult{{Tree: "repo1", Path: "main.go"}},
			}, nil
		},
	})
	res := callTool(t, s, "search_files", map[string]interface{}{"pattern": "main"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content[0].Text)
	}
	if !gotQuery.FilenameOnly {
		t.Error("search_files should set FilenameOnly")
	}
	var payload FileSearchResponse
	if err := json.Unmarshal([]byte(res.Content[0].Text), &payload); err != nil {
		t.Fatalf("invalid JSON payload: %v", err)
	}
	if len(payload.Results) != 1 || payload.Results[0].Path != "main.go" {
		t.Errorf("results = %+v", payload.Results)
	}
}

func TestGetRepos(t *testing.T) {
	s := NewServer(&fakeClient{
		info: func(*pb.InfoRequest) (*pb.ServerInfo, error) {
			return &pb.ServerInfo{Trees: []*pb.ServerInfo_Tree{{Name: "a"}, {Name: "b"}}}, nil
		},
	})
	res := callTool(t, s, "get_repos", nil)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content[0].Text)
	}
	var payload RepoResponse
	if err := json.Unmarshal([]byte(res.Content[0].Text), &payload); err != nil {
		t.Fatalf("invalid JSON payload: %v", err)
	}
	if len(payload.Repos) != 2 {
		t.Errorf("got %d repos, want 2", len(payload.Repos))
	}
}

func TestUnknownTool(t *testing.T) {
	s := NewServer(&fakeClient{})
	resp := s.handleRequest(context.Background(), newRequest(t, 1, "tools/call", ToolCallRequest{Name: "nope"}))
	if resp == nil || resp.Error == nil {
		t.Fatalf("expected error response, got %+v", resp)
	}
}

func TestServeParseError(t *testing.T) {
	s := NewServer(&fakeClient{})
	var out strings.Builder
	if err := s.Serve(context.Background(), strings.NewReader("{not json}\n"), &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	var resp Response
	if err := json.Unmarshal([]byte(out.String()), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != errParse {
		t.Errorf("expected parse error, got %+v", resp.Error)
	}
}

func TestServeNotificationProducesNoOutput(t *testing.T) {
	s := NewServer(&fakeClient{})
	var out strings.Builder
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	if out.String() != "" {
		t.Errorf("notification produced output: %q", out.String())
	}
}
