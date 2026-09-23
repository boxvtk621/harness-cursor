package nodesettings

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

const maxMCPCheckResponse = 1 << 20

type mcpRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

// probeMCP checks a draft endpoint independently of the Cursor SDK worker.
// It says whether the endpoint answered now; it cannot prove SDK tool use.
func probeMCP(parent context.Context, server privateMCP) (int, string) {
	timeout := time.Duration(server.TimeoutMS) * time.Millisecond
	if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	protocol, session := "", ""
	call := func(id int, method string, params any, notification bool) (json.RawMessage, string) {
		payload := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
		if !notification {
			payload["id"] = id
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, "mcp_protocol_invalid"
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
		if err != nil {
			return nil, "mcp_connect_failed"
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		if server.BearerToken != "" {
			request.Header.Set("Authorization", "Bearer "+server.BearerToken)
		}
		if protocol != "" {
			request.Header.Set("MCP-Protocol-Version", protocol)
		}
		if session != "" {
			request.Header.Set("Mcp-Session-Id", session)
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, "mcp_connect_failed"
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode > 299 {
			return nil, "mcp_connect_failed"
		}
		if responseSession := response.Header.Get("Mcp-Session-Id"); responseSession != "" {
			if strings.ContainsAny(responseSession, "\r\n\x00") || len(responseSession) > 512 {
				return nil, "mcp_protocol_invalid"
			}
			session = responseSession
		}
		if notification {
			return nil, ""
		}
		mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
		var limited []byte
		if mediaType == "text/event-stream" {
			scanner := bufio.NewScanner(io.LimitReader(response.Body, maxMCPCheckResponse+1))
			scanner.Buffer(make([]byte, 4096), maxMCPCheckResponse)
			data := []string{}
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "data:") {
					data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				}
				if line == "" && len(data) != 0 {
					candidate := []byte(strings.Join(data, "\n"))
					var frame mcpRPCResponse
					if json.Unmarshal(candidate, &frame) == nil && frame.ID == id {
						limited = candidate
						break
					}
					data = nil
				}
			}
			if scanner.Err() != nil || len(limited) == 0 {
				return nil, "mcp_protocol_invalid"
			}
		} else if mediaType == "application/json" {
			var err error
			limited, err = io.ReadAll(io.LimitReader(response.Body, maxMCPCheckResponse+1))
			if err != nil || len(limited) > maxMCPCheckResponse {
				return nil, "mcp_protocol_invalid"
			}
		} else {
			return nil, "mcp_protocol_invalid"
		}
		var rpc mcpRPCResponse
		if json.Unmarshal(limited, &rpc) != nil || rpc.JSONRPC != "2.0" || rpc.ID != id || len(rpc.Result) == 0 || len(rpc.Error) != 0 && string(rpc.Error) != "null" {
			return nil, "mcp_protocol_invalid"
		}
		return rpc.Result, ""
	}
	initial, reason := call(1, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "harness-mcp-check", "version": "1"}}, false)
	if reason != "" {
		return 0, reason
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(initial, &initialized) != nil || initialized.ProtocolVersion == "" || len(initialized.ProtocolVersion) > 32 {
		return 0, "mcp_protocol_invalid"
	}
	protocol = initialized.ProtocolVersion
	if _, reason = call(0, "notifications/initialized", map[string]any{}, true); reason != "" {
		return 0, reason
	}
	count := 0
	cursor := ""
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, reason := call(page+2, "tools/list", params, false)
		if reason != "" {
			return 0, reason
		}
		var listing struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if json.Unmarshal(result, &listing) != nil || listing.Tools == nil || len(listing.Tools) > 1000 || len(listing.NextCursor) > 512 {
			return 0, "mcp_protocol_invalid"
		}
		for _, tool := range listing.Tools {
			if !validText(tool.Name, 200) {
				return 0, "mcp_protocol_invalid"
			}
		}
		count += len(listing.Tools)
		if listing.NextCursor == "" {
			return count, ""
		}
		if listing.NextCursor == cursor {
			return 0, "mcp_protocol_invalid"
		}
		cursor = listing.NextCursor
	}
	return 0, "mcp_protocol_invalid"
}
