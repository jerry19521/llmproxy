// anthropic.go：Anthropic Messages API 格式（/v1/messages）的 IR 解析与构建。
package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ===== 请求解析（Anthropic → IR）=====

func parseAnthropicRequest(body []byte) (*RequestIR, error) {
	var req struct {
		Model    string          `json:"model"`
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MaxTokens     int      `json:"max_tokens"`
		Temperature   *float64 `json:"temperature"`
		TopP          *float64 `json:"top_p"`
		StopSequences []string `json:"stop_sequences"`
		Stream        bool     `json:"stream"`
		Tools         []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	ir := &RequestIR{
		Model:       req.Model,
		Stream:      req.Stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.StopSequences,
		System:      parseAnthropicSystem(req.System),
	}
	for _, t := range req.Tools {
		ir.Tools = append(ir.Tools, ToolDef{Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
	}
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "auto", "any", "none":
			ir.ToolChoice = &ToolChoice{Type: req.ToolChoice.Type}
		case "tool":
			ir.ToolChoice = &ToolChoice{Type: "tool", Name: req.ToolChoice.Name}
		}
	}
	for i, m := range req.Messages {
		blocks, err := parseAnthropicContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		var toolMsgs []InputMessage
		var parts []ContentPart
		var text strings.Builder
		var toolCalls []ToolCall
		for _, b := range blocks {
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
				parts = append(parts, ContentPart{Type: "text", Text: b.Text})
			case "image":
				if b.Source != nil {
					switch b.Source.Type {
					case "base64":
						parts = append(parts, ContentPart{
							Type: "image", ImageB64: b.Source.Data, ImageMime: b.Source.MediaType,
						})
					case "url":
						parts = append(parts, ContentPart{Type: "image", ImageURL: b.Source.URL})
					}
					// 其他 source 类型（如 file）不参与跨格式转换：Anthropic 侧无法表示，跳过
					// （旧实现落入 URL 分支产出空链接，修复为静默丢弃）
				}
			case "tool_use":
				toolCalls = append(toolCalls, ToolCall{ID: b.ID, Name: b.Name, Args: string(b.Input)})
			case "tool_result":
				toolMsgs = append(toolMsgs, InputMessage{
					Role: "tool", ToolCallID: b.ToolUseID, Text: rawJSONToText(b.Content),
				})
			}
		}
		switch m.Role {
		case "assistant":
			msg := InputMessage{Role: "assistant", Text: text.String(), ToolCalls: toolCalls}
			if len(parts) > 0 && text.Len() == 0 {
				msg.Parts = parts
			}
			ir.Messages = append(ir.Messages, msg)
		default: // user
			// v1.0.0 L11 修复：按块原始顺序交错展开文本段与 tool_result（旧实现 toolMsgs 先行缓冲，
			// 会把同一消息内 [text, tool_result] 重排为 [tool_result, text]）；
			// 后续构建阶段相邻同角色自动合并，最终块序与输入一致
			var seq []InputMessage
			curText := strings.Builder{}
			var curParts []ContentPart
			flushUser := func() {
				if curText.Len() > 0 || len(curParts) > 0 {
					msg := InputMessage{Role: "user", Text: curText.String()}
					if len(curParts) > 0 {
						msg.Parts = curParts
					}
					seq = append(seq, msg)
				}
				curText.Reset()
				curParts = nil
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					curText.WriteString(b.Text)
					// v1.0.0（H1）：text 块同时进 Parts——IR 不变式为「Parts 非空时 Parts 覆盖
					// 全部内容」（CC/Responses 解析器均如此），否则构建层 Parts 优先会静默丢弃文本：
					// Anthropic 图文混排消息 [text, image] 转到 OpenAI/Responses 上游时，
					// 用户的问题本身没发给模型，回答完全跑偏且无任何日志提示。
					curParts = append(curParts, ContentPart{Type: "text", Text: b.Text})
				case "image":
					if b.Source != nil {
						switch b.Source.Type {
						case "base64":
							curParts = append(curParts, ContentPart{Type: "image", ImageB64: b.Source.Data, ImageMime: b.Source.MediaType})
						case "url":
							curParts = append(curParts, ContentPart{Type: "image", ImageURL: b.Source.URL})
						}
					}
				case "tool_result":
					flushUser()
					seq = append(seq, InputMessage{Role: "tool", ToolCallID: b.ToolUseID, Text: rawJSONToText(b.Content)})
				}
			}
			flushUser()
			if len(seq) == 0 && len(toolMsgs) > 0 {
				seq = append(seq, toolMsgs...) // 理论不可达（同一消息内已处理），双保险
			}
			ir.Messages = append(ir.Messages, seq...)
		}
	}
	return ir, nil
}

// parseAnthropicSystem 解析 system 字段：字符串或文本块数组。
func parseAnthropicSystem(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var texts []string
		for _, b := range blocks {
			if (b.Type == "text" || b.Type == "") && b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
		return strings.Join(texts, "\n\n")
	}
	return ""
}

// antBlock Anthropic 内容块（联合结构）。
type antBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// thinking 块（v1.0.0 M6）：思考内容；signature 跨格式不可重建，解析时丢弃
	Thinking string `json:"thinking"`
	// tool_use（assistant）
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result（user）
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	// image
	Source *antImageSource `json:"source"`
}

type antImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// parseAnthropicContent 解析 content：字符串或内容块数组。
func parseAnthropicContent(raw json.RawMessage) ([]antBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []antBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []antBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("unsupported content format")
	}
	return blocks, nil
}

// ===== 请求构建（IR → Anthropic）=====

func buildAnthropicRequest(ir *RequestIR, model string) ([]byte, error) {
	maxTokens := ir.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096 // Anthropic max_tokens 必填，未提供时给默认值
	}
	m := map[string]interface{}{
		"model":      model,
		"messages":   buildAnthropicMessages(ir),
		"max_tokens": maxTokens,
		"stream":     ir.Stream,
	}
	if ir.System != "" {
		m["system"] = ir.System
	}
	if ir.Temperature != nil {
		m["temperature"] = *ir.Temperature
	}
	if ir.TopP != nil {
		m["top_p"] = *ir.TopP
	}
	if len(ir.Stop) > 0 {
		m["stop_sequences"] = ir.Stop
	}
	if len(ir.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(ir.Tools))
		for _, t := range ir.Tools {
			schema := t.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]interface{}{
				"name": t.Name, "description": t.Description, "input_schema": schema,
			})
		}
		m["tools"] = tools
		// tool_choice 省略时 Anthropic 默认 auto；
		// OpenAI 的 "none" 无直接对应（旧实现直译 {"type":"none"}，
		// Anthropic 官方不支持该取值会 400，v1.0.0 L11 修复为省略字段回退默认 auto）
		if tc := anthropicToolChoiceValue(ir.ToolChoice); tc != nil {
			m["tool_choice"] = tc
		}
	}
	return json.Marshal(m)
}

// buildAnthropicMessages 由 IR 生成 Anthropic messages。
// 相邻同角色消息合并（tool 消息转为带 tool_result 块的 user 消息），保证角色交替结构。
func buildAnthropicMessages(ir *RequestIR) []map[string]interface{} {
	type antMsg struct {
		role   string
		blocks []map[string]interface{}
	}
	var out []*antMsg
	appendBlock := func(role string, block map[string]interface{}) {
		if n := len(out); n > 0 && out[n-1].role == role {
			out[n-1].blocks = append(out[n-1].blocks, block)
			return
		}
		out = append(out, &antMsg{role: role, blocks: []map[string]interface{}{block}})
	}
	for _, msg := range ir.Messages {
		switch msg.Role {
		case "tool":
			appendBlock("user", map[string]interface{}{
				"type": "tool_result", "tool_use_id": msg.ToolCallID, "content": msg.Text,
			})
		case "assistant":
			if msg.Text != "" {
				appendBlock("assistant", map[string]interface{}{"type": "text", "text": msg.Text})
			}
			for _, c := range msg.ToolCalls {
				appendBlock("assistant", map[string]interface{}{
					"type": "tool_use", "id": c.ID, "name": c.Name, "input": rawArgs(c.Args),
				})
			}
		case "system":
			// IR 系统词统一走顶层 system 字段，忽略
		default: // user
			if len(msg.Parts) > 0 {
				for _, p := range msg.Parts {
					if p.Type == "image" {
						appendBlock("user", map[string]interface{}{"type": "image", "source": anthropicImageSource(p)})
					} else if p.Text != "" {
						appendBlock("user", map[string]interface{}{"type": "text", "text": p.Text})
					}
				}
			} else if msg.Text != "" {
				appendBlock("user", map[string]interface{}{"type": "text", "text": msg.Text})
			}
		}
	}
	msgs := make([]map[string]interface{}, 0, len(out))
	for _, m := range out {
		// 单一文本块时使用纯字符串 content（与原生客户端一致，更简洁）
		if len(m.blocks) == 1 && m.blocks[0]["type"] == "text" {
			msgs = append(msgs, map[string]interface{}{"role": m.role, "content": m.blocks[0]["text"]})
			continue
		}
		msgs = append(msgs, map[string]interface{}{"role": m.role, "content": m.blocks})
	}
	// L11 防御：Anthropic 要求首条消息必须为 user 且 messages 非空。
	// 极端输入（如 CC 解析出空文本消息被跳过）可能产出空列表或 assistant 开头，
	// 插入占位 user 消息避免上游 400。
	if len(msgs) == 0 {
		msgs = append(msgs, map[string]interface{}{"role": "user", "content": "(empty)"})
	} else if msgs[0]["role"] != "user" {
		msgs = append([]map[string]interface{}{{"role": "user", "content": "(empty)"}}, msgs...)
	}
	return msgs
}

func anthropicImageSource(p ContentPart) map[string]interface{} {
	if p.ImageB64 != "" {
		mime := p.ImageMime
		if mime == "" {
			mime = "image/png"
		}
		return map[string]interface{}{"type": "base64", "media_type": mime, "data": p.ImageB64}
	}
	return map[string]interface{}{"type": "url", "url": p.ImageURL}
}

// anthropicToolChoiceValue 把 IR 工具选择映射为 Anthropic tool_choice。
// 返回 nil 表示省略该字段（回退 Anthropic 默认 auto）。
func anthropicToolChoiceValue(tc *ToolChoice) interface{} {
	if tc == nil {
		return map[string]string{"type": "auto"}
	}
	switch tc.Type {
	case "none":
		// Anthropic 官方 tool_choice 不支持 none：省略字段回退默认 auto（v1.0.0 L11 修复）
		return nil
	case "any":
		return map[string]string{"type": "any"}
	case "tool":
		return map[string]string{"type": "tool", "name": tc.Name}
	default:
		return map[string]string{"type": "auto"}
	}
}

// ===== 响应解析（Anthropic → IR）=====

func parseAnthropicResponse(body []byte) (*ResponseIR, error) {
	var resp struct {
		ID         string     `json:"id"`
		Type       string     `json:"type"`
		Role       string     `json:"role"`
		Model      string     `json:"model"`
		Content    []antBlock `json:"content"`
		StopReason string     `json:"stop_reason"`
		Usage      *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("upstream error: %s", resp.Error.Message)
	}
	ir := &ResponseIR{
		ID:     resp.ID,
		Model:  resp.Model,
		Finish: anthropicToFinish(resp.StopReason),
	}
	var texts []string
	var reasons []string
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "thinking":
			// v1.0.0（M6）：extended thinking 块归一入 Reasoning 通道；
			// redacted_thinking（加密思考）无法转文本，跳过
			if b.Thinking != "" {
				reasons = append(reasons, b.Thinking)
			}
		case "tool_use":
			ir.ToolCalls = append(ir.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Args: string(b.Input)})
		}
	}
	ir.Text = strings.Join(texts, "")
	ir.Reasoning = strings.Join(reasons, "")
	if resp.Usage != nil {
		ir.PromptTokens = resp.Usage.InputTokens
		ir.CompletionTokens = resp.Usage.OutputTokens
		ir.TotalTokens = ir.PromptTokens + ir.CompletionTokens
	}
	return ir, nil
}

// ===== 响应构建（IR → Anthropic）=====

func buildAnthropicResponse(ir *ResponseIR, model string) []byte {
	if model == "" {
		model = ir.Model
	}
	content := []map[string]interface{}{}
	// v1.0.0（M6）：思考内容前置 thinking 块——signature 跨格式不可重建（留空，
	// 客户端展示用途不校验；客户端回传历史时我们的请求解析器忽略 thinking 块，
	// 不会携带无效 signature 回上游）。
	if ir.Reasoning != "" {
		content = append(content, map[string]interface{}{
			"type": "thinking", "thinking": ir.Reasoning, "signature": "",
		})
	}
	if ir.Text != "" || (len(ir.ToolCalls) == 0 && ir.Reasoning == "") {
		content = append(content, map[string]interface{}{"type": "text", "text": ir.Text})
	}
	for _, c := range ir.ToolCalls {
		content = append(content, map[string]interface{}{
			"type": "tool_use", "id": c.ID, "name": c.Name, "input": rawArgs(c.Args),
		})
	}
	resp := map[string]interface{}{
		"id":            nonEmpty(ir.ID, "msg_llmproxy"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   finishToAnthropic(ir.Finish),
		"stop_sequence": nil,
		"usage": map[string]int64{
			"input_tokens":  ir.PromptTokens,
			"output_tokens": ir.CompletionTokens,
		},
	}
	b, _ := json.Marshal(resp)
	return b
}
