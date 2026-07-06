package mcphost

import (
	"bufio"
	"encoding/json"
	"io"
)

// handle reads the preamble, authenticates the token, then runs the MCP
// loop over the SAME buffered reader so bytes the client pipelined after
// the preamble are not lost. Any auth failure simply closes the conn.
func (h *Host) handle(c io.ReadWriteCloser) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 1<<20)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return // client hung up before sending a preamble
	}
	var pre struct {
		Token string `json:"token"`
	}
	if jerr := json.Unmarshal(line, &pre); jerr != nil {
		return // malformed preamble
	}
	sessionKey, ok := h.resolve(pre.Token)
	if !ok {
		return // unknown token
	}
	_ = h.runMCP(br, c, sessionKey)
}

// rpcMessage is a minimal JSON-RPC 2.0 envelope (request, response, or
// notification — distinguished by which fields are set).
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent => notification
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const mcpProtocolVersion = "2025-06-18"

// runMCP runs the MCP server loop over r/w until r hits EOF. Each
// tools/call is dispatched to a registered handler with sessionKey bound.
// If r is already a *bufio.Reader it is reused so no bytes buffered ahead
// (e.g. after a preamble read) are lost.
func (h *Host) runMCP(r io.Reader, w io.Writer, sessionKey string) error {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 1<<20)
	}
	enc := json.NewEncoder(w)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if rerr := h.handleLine(line, enc, sessionKey); rerr != nil {
				return rerr
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

func (h *Host) handleLine(line []byte, enc *json.Encoder, sessionKey string) error {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil // ignore non-JSON / blank lines
	}
	if msg.Method == "" || len(msg.ID) == 0 {
		return nil // notification or response; nothing to reply to
	}
	resp := rpcMessage{JSONRPC: "2.0", ID: msg.ID}
	switch msg.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": h.cfg.ServerInfoName, "version": h.cfg.ServerInfoVersion},
		}
	case "tools/list":
		resp.Result = map[string]any{"tools": h.toolSpecs()}
	case "tools/call":
		resp.Result = h.handleToolCall(msg.Params, sessionKey)
	case "ping":
		resp.Result = map[string]any{}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + msg.Method}
	}
	return enc.Encode(&resp)
}

// toolSpecs returns the JSON-RPC tool specs advertised to the agent, in
// registration order.
func (h *Host) toolSpecs() []any {
	specs := make([]any, 0, len(h.toolsIn))
	for _, t := range h.toolsIn {
		specs = append(specs, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.inputSchema,
		})
	}
	return specs
}

func (h *Host) handleToolCall(params json.RawMessage, sessionKey string) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return toolError("invalid params: " + err.Error())
	}
	t, ok := h.tools[p.Name]
	if !ok {
		return toolError("unknown tool: " + p.Name)
	}
	text, err := t.handler(sessionKey, p.Arguments)
	if err != nil {
		return toolError(err.Error())
	}
	return okText(text)
}

func okText(text string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
}

func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": msg}},
		"isError": true,
	}
}
