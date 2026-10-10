// openai.go：OpenAI Chat Completions 格式（/v1/chat/completions）的 IR 解析与构建。
package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ===== 请求解析（OpenAI → IR）=====

func parseOpenAIRequest(body []byte) (*RequestIR, error) {
	var req struct {
		Model            string            `json:"model"`
		Messages         []json.RawMessage `json:"messages"`
		MaxTokens        int               `json:"max_tokens"`
		MaxCompletionTok int               `json:"max_completion_tokens"`
		Temperature      *float64          `json:"temperature"`
		TopP             *float64          `json:"top_p"`
		Stop             json.RawMessage   `json:"stop"`
		Stream           bool              `json:"stream"`
		Tools            []struct {
			Type     string `json:"type"`
			Function *struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice     json.RawMessage `json:"tool_choice"`
		ResponseFormat *struct {
			Type string `json:"type"`
		} `json:"response_format"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	ir := &RequestIR{
		Model:           req.Model,
		Stream:          req.Stream,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		ReasoningEffort: req.ReasoningEffort,
	}
	if req.MaxTokens > 0 {
		ir.MaxTokens = req.MaxTokens
	} else if req.MaxCompletionTok > 0 {
		ir.MaxTokens = req.MaxCompletionTok
	}
	ir.Stop = parseStop(req.Stop)
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object" {
		ir.JSONMode = true
	}
	for _, t := range req.Tools {
		if t.Function == nil {
			continue // 非函数工具（如自定义工具）暂不支持转换
		}
		ir.Tools = append(ir.Tools, ToolDef{
			Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters,
		})
	}
	ir.ToolChoice = parseToolChoiceFlat(req.ToolChoice)

	var sysTexts []string
	for i, raw := range req.Messages {
		var m struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch m.Role {
		case "system", "developer":
			text, _, _, err := parseOpenAIContent(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			if text != "" {
				sysTexts = append(sysTexts, text)
			}
		case "tool":
			text, _, _, err := parseOpenAIContent(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			ir.Messages = append(ir.Messages, InputMessage{Role: "tool", ToolCallID: m.ToolCallID, Text: text})
		case "user", "assistant", "":
			role := m.Role
			if role == "" {
				role = "user"
			}
			text, parts, hasParts, err := parseOpenAIContent(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			msg := InputMessage{Role: role, Text: text}
			if hasParts {
				msg.Parts = parts
			}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
			}
			ir.Messages = append(ir.Messages, msg)
		default:
			// 未知角色（如 function，旧语法）：按 user 文本处理
			text, _, _, err := parseOpenAIContent(m.Content)
			if err == nil && text != "" {
				ir.Messages = append(ir.Messages, InputMessage{Role: "user", Text: text})
			}
		}
	}
	ir.System = strings.Join(sysTexts, "\n\n")
	return ir, nil
}

// parseOpenAIContent 解析 CC 消息 content：字符串或片段数组（text / image_url）。
func parseOpenAIContent(raw json.RawMessage) (text string, parts []ContentPart, hasParts bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, false, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil, false, nil
	}
	var arr []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if json.Unmarshal(raw, &arr) == nil {
		var sb strings.Builder
		for _, p := range arr {
			switch p.Type {
			case "text", "":
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(p.Text)
				parts = append(parts, ContentPart{Type: "text", Text: p.Text})
			case "image_url":
				// 官方为对象形式 {"url":...}；兼容部分客户端的纯字符串形式
				if u := parseImageURLField(p.ImageURL); u != "" {
					parts = append(parts, imagePartFromURL(u))
				}
			}
			// 其他片段类型（audio/file 等）暂不转换
		}
		return sb.String(), parts, len(parts) > 0, nil
	}
	return "", nil, false, fmt.Errorf("unsupported content format")
}

// ===== 请求构建（IR → OpenAI）=====

func buildOpenAIRequest(ir *RequestIR, model string) ([]byte, error) {
	m := map[string]interface{}{
		"model":    model,
		"messages": buildOpenAIMessages(ir),
		"stream":   ir.Stream,
	}
	if ir.Stream {
		// 流式时请求上游在末 chunk 携带 usage（OpenAI 官方字段，主流兼容网关均支持），
		// 保证跨格式流式转换的 token 统计可用。
		m["stream_options"] = map[string]interface{}{"include_usage": true}
	}
	if ir.MaxTokens > 0 {
		m["max_tokens"] = ir.MaxTokens
	}
	if ir.Temperature != nil {
		m["temperature"] = *ir.Temperature
	}
	if ir.TopP != nil {
		m["top_p"] = *ir.TopP
	}
	if len(ir.Stop) > 0 {
		m["stop"] = ir.Stop
	}
	if len(ir.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(ir.Tools))
		for _, t := range ir.Tools {
			fn := map[string]interface{}{"name": t.Name, "description": t.Description}
			if len(t.Parameters) > 0 {
				fn["parameters"] = t.Parameters
			}
			tools = append(tools, map[string]interface{}{"type": "function", "function": fn})
		}
		m["tools"] = tools
		m["tool_choice"] = openAIToolChoiceValue(ir.ToolChoice)
	}
	if ir.JSONMode {
		m["response_format"] = map[string]string{"type": "json_object"}
	}
	if ir.ReasoningEffort != "" {
		m["reasoning_effort"] = ir.ReasoningEffort
	}
	return json.Marshal(m)
}

// buildOpenAIMessages 由 IR 生成 CC messages。
func buildOpenAIMessages(ir *RequestIR) []map[string]interface{} {
	out := []map[string]interface{}{}
	if ir.System != "" {
		out = append(out, map[string]interface{}{"role": "system", "content": ir.System})
	}
	for _, msg := range ir.Messages {
		switch msg.Role {
		case "system":
			if msg.Text != "" {
				out = append(out, map[string]interface{}{"role": "system", "content": msg.Text})
			}
		case "tool":
			out = append(out, map[string]interface{}{
				"role": "tool", "tool_call_id": msg.ToolCallID, "content": msg.Text,
			})
		case "assistant":
			m := map[string]interface{}{"role": "assistant"}
			// v1.8.4（C7）：openAIParts 过滤空 text 片段后可能为空，回退纯文本通道
			if parts := openAIParts(msg.Parts); len(parts) > 0 {
				m["content"] = parts
			} else if msg.Text != "" {
				m["content"] = msg.Text
			} else {
				m["content"] = nil
			}
			if len(msg.ToolCalls) > 0 {
				m["tool_calls"] = openAIToolCalls(msg.ToolCalls)
			}
			out = append(out, m)
		default: // user
			// v1.8.4（C7）：过滤空 text 片段；全被过滤时回退 Text（避免产出空 content 数组）
			if parts := openAIParts(msg.Parts); len(parts) > 0 {
				out = append(out, map[string]interface{}{"role": "user", "content": parts})
			} else {
				out = append(out, map[string]interface{}{"role": "user", "content": msg.Text})
			}
		}
	}
	return out
}

func openAIParts(parts []ContentPart) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(parts))
	for _, p := range parts {
		if p.Type == "image" {
			out = append(out, map[string]interface{}{
				"type":      "image_url",
				"image_url": map[string]string{"url": imageDataURL(p)},
			})
		} else if p.Text != "" {
			// v1.8.4（C7）：空 text 片段跳过（与 Anthropic 构建层对齐）——
			// 部分客户端发 [{"type":"text","text":""},{"image_url"}] 空占位写法，
			// 不过滤时产出 {"type":"text","text":""} 空片段，严格网关会 400。
			out = append(out, map[string]interface{}{"type": "text", "text": p.Text})
		}
	}
	return out
}

func openAIToolCalls(calls []ToolCall) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(calls))
	for _, c := range calls {
		out = append(out, map[string]interface{}{
			"id":   c.ID,
			"type": "function",
			"function": map[string]string{
				"name":      c.Name,
				"arguments": c.Args,
			},
		})
	}
	return out
}

func openAIToolChoiceValue(tc *ToolChoice) interface{} {
	if tc == nil {
		return "auto"
	}
	switch tc.Type {
	case "tool":
		return map[string]interface{}{"type": "function", "function": map[string]string{"name": tc.Name}}
	case "any":
		return "required"
	case "none":
		return "none"
	default:
		return "auto"
	}
}

// ===== 响应解析（OpenAI → IR）=====

func parseOpenAIResponse(body []byte) (*ResponseIR, error) {
	var resp struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string  `json:"role"`
				Content          *string `json:"content"`
				ReasoningContent string  `json:"reasoning_content"` // v1.8.4（M6）：DeepSeek R1/兼容网关惯例
				Reasoning        string  `json:"reasoning"`         // v1.8.4（M6）：部分网关短字段名
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("upstream error: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("upstream response has no choices: %s", openAIErrorMessage(body))
	}
	ir := &ResponseIR{ID: resp.ID, Model: resp.Model, Created: resp.Created}
	ch := resp.Choices[0]
	if ch.Message.Content != nil {
		ir.Text = *ch.Message.Content
	}
	// v1.8.4（M6）：思考内容归一入 Reasoning 通道（reasoning_content 优先）
	ir.Reasoning = nonEmpty(ch.Message.ReasoningContent, ch.Message.Reasoning)
	for _, tc := range ch.Message.ToolCalls {
		ir.ToolCalls = append(ir.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
	}
	ir.Finish = "stop"
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		ir.Finish = *ch.FinishReason
	}
	if resp.Usage != nil {
		ir.PromptTokens = resp.Usage.PromptTokens
		ir.CompletionTokens = resp.Usage.CompletionTokens
		ir.TotalTokens = resp.Usage.TotalTokens
	}
	if ir.TotalTokens == 0 {
		ir.TotalTokens = ir.PromptTokens + ir.CompletionTokens
	}
	return ir, nil
}

// ===== 响应构建（IR → OpenAI）=====

func buildOpenAIResponse(ir *ResponseIR, model string) []byte {
	if model == "" {
		model = ir.Model
	}
	created := ir.Created
	if created == 0 {
		created = nowUnix()
	}
	msg := map[string]interface{}{"role": "assistant"}
	if ir.Text != "" {
		msg["content"] = ir.Text
	} else {
		msg["content"] = nil
	}
	// v1.8.4（M6）：思考内容转发（DeepSeek R1/兼容网关惯例字段；官方上游忽略未知字段）
	if ir.Reasoning != "" {
		msg["reasoning_content"] = ir.Reasoning
	}
	if len(ir.ToolCalls) > 0 {
		msg["tool_calls"] = openAIToolCalls(ir.ToolCalls)
	}
	finish := ir.Finish
	if finish == "" {
		finish = "stop"
	}
	resp := map[string]interface{}{
		"id":      nonEmpty(ir.ID, "chatcmpl-llmproxy"),
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": map[string]int64{
			"prompt_tokens":     ir.PromptTokens,
			"completion_tokens": ir.CompletionTokens,
			"total_tokens":      ir.TotalTokens,
		},
	}
	b, _ := json.Marshal(resp)
	return b
}
