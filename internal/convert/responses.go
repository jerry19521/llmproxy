// responses.go：OpenAI Responses API 格式（/v1/responses）的 IR 解析与构建。
package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ===== 请求解析（Responses → IR）=====

func parseResponsesRequest(body []byte) (*RequestIR, error) {
	var req struct {
		Model           string          `json:"model"`
		Input           json.RawMessage `json:"input"`
		Instructions    string          `json:"instructions"`
		MaxOutputTokens int             `json:"max_output_tokens"`
		Temperature     *float64        `json:"temperature"`
		TopP            *float64        `json:"top_p"`
		Stream          bool            `json:"stream"`
		Tools           []struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
		Text       *struct {
			Format *struct {
				Type string `json:"type"`
			} `json:"format"`
		} `json:"text"`
		Reasoning *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	ir := &RequestIR{
		Model:       req.Model,
		Stream:      req.Stream,
		MaxTokens:   req.MaxOutputTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		System:      req.Instructions,
	}
	if req.Text != nil && req.Text.Format != nil && req.Text.Format.Type == "json_object" {
		ir.JSONMode = true
	}
	if req.Reasoning != nil {
		ir.ReasoningEffort = req.Reasoning.Effort
	}
	for _, t := range req.Tools {
		if t.Type == "" || t.Type == "function" {
			ir.Tools = append(ir.Tools, ToolDef{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
		}
	}
	ir.ToolChoice = parseToolChoiceFlat(req.ToolChoice)

	// input：字符串 → 单条 user 消息；数组 → 条目列表
	var s string
	if len(req.Input) > 0 && json.Unmarshal(req.Input, &s) == nil {
		ir.Messages = append(ir.Messages, InputMessage{Role: "user", Text: s})
		return ir, nil
	}
	var items []json.RawMessage
	if len(req.Input) > 0 {
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, fmt.Errorf("input: %w", err)
		}
	}
	var sysTexts []string
	for i, raw := range items {
		var it struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Output    json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(raw, &it); err != nil {
			return nil, fmt.Errorf("input[%d]: %w", i, err)
		}
		switch {
		case it.Type == "function_call":
			appendAssistantToolCall(ir, it.CallID, it.Name, it.Arguments)
		case it.Type == "function_call_output":
			ir.Messages = append(ir.Messages, InputMessage{
				Role: "tool", ToolCallID: it.CallID, Text: rawJSONToText(it.Output),
			})
		case it.Type == "reasoning" || it.Type == "image_generation_call" ||
			it.Type == "code_interpreter_call" || it.Type == "web_search_call" ||
			it.Type == "file_search_call" || it.Type == "mcp_call" || it.Type == "mcp_list_tools":
			// 推理与内置工具条目不参与跨格式转换，忽略
		default:
			switch it.Role {
			case "system", "developer":
				text, _, _ := parseResponsesContent(it.Content)
				if text != "" {
					sysTexts = append(sysTexts, text)
				}
			case "user", "assistant", "":
				text, parts, err := parseResponsesContent(it.Content)
				if err != nil {
					return nil, fmt.Errorf("input[%d]: %w", i, err)
				}
				role := it.Role
				if role == "" {
					role = "user"
				}
				if role == "assistant" {
					appendAssistantText(ir, text, parts)
				} else {
					msg := InputMessage{Role: "user", Text: text}
					if len(parts) > 0 {
						msg.Parts = parts
					}
					ir.Messages = append(ir.Messages, msg)
				}
			}
		}
	}
	if len(sysTexts) > 0 {
		if ir.System != "" {
			ir.System = ir.System + "\n\n" + strings.Join(sysTexts, "\n\n")
		} else {
			ir.System = strings.Join(sysTexts, "\n\n")
		}
	}
	return ir, nil
}

// parseResponsesContent 解析 Responses content：字符串或片段数组
// （input_text / output_text / summary_text / input_image）。
func parseResponsesContent(raw json.RawMessage) (string, []ContentPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil, nil
	}
	var arr []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if json.Unmarshal(raw, &arr) == nil {
		var sb strings.Builder
		var parts []ContentPart
		for _, p := range arr {
			switch p.Type {
			case "input_text", "output_text", "summary_text", "":
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(p.Text)
				parts = append(parts, ContentPart{Type: "text", Text: p.Text})
			case "input_image":
				// 官方为字符串形式；兼容部分客户端的对象形式 {"url":...,"detail":...}
				if u := parseImageURLField(p.ImageURL); u != "" {
					parts = append(parts, imagePartFromURL(u))
				}
			}
		}
		return sb.String(), parts, nil
	}
	return "", nil, fmt.Errorf("unsupported content format")
}

// parseImageURLField 解析 image_url 字段：官方为字符串形式；
// 兼容对象形式 {"url":"...","detail":"..."}（detail 等附加字段忽略，不丢图片）。
func parseImageURLField(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.URL
	}
	return ""
}

// rawJSONToText 将 output/content 字段转为文本。
// 支持三种形式：字符串、内容块数组（Anthropic tool_result.content）、裸 JSON 对象。
// 块数组形式提取 text 块文本拼接，非 text 块（image 等）因 IR tool 消息仅有 Text 字段而跳过。
func rawJSONToText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// 块数组形式（Anthropic tool_result.content: [{"type":"text","text":"..."},...]）
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
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
		// 数组中无 text 块（如纯 image）：返回空而非原始 JSON
		return ""
	}
	return string(raw)
}

// ===== 请求构建（IR → Responses）=====

func buildResponsesRequest(ir *RequestIR, model string) ([]byte, error) {
	m := map[string]interface{}{
		"model":  model,
		"stream": ir.Stream,
	}
	input := buildResponsesInput(ir)
	if len(input) > 0 {
		m["input"] = input
	}
	if ir.System != "" {
		m["instructions"] = ir.System
	}
	if ir.MaxTokens > 0 {
		m["max_output_tokens"] = ir.MaxTokens
	}
	if ir.Temperature != nil {
		m["temperature"] = *ir.Temperature
	}
	if ir.TopP != nil {
		m["top_p"] = *ir.TopP
	}
	if len(ir.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(ir.Tools))
		for _, t := range ir.Tools {
			fn := map[string]interface{}{"type": "function", "name": t.Name, "description": t.Description}
			if len(t.Parameters) > 0 {
				fn["parameters"] = t.Parameters
			}
			tools = append(tools, fn)
		}
		m["tools"] = tools
		m["tool_choice"] = responsesToolChoiceValue(ir.ToolChoice)
	}
	if ir.JSONMode {
		m["text"] = map[string]interface{}{"format": map[string]string{"type": "json_object"}}
	}
	if ir.ReasoningEffort != "" {
		m["reasoning"] = map[string]string{"effort": ir.ReasoningEffort}
	}
	// 说明：stop 序列无 Responses 等价参数，丢弃
	return json.Marshal(m)
}

// buildResponsesInput 由 IR 生成 Responses input 条目。
func buildResponsesInput(ir *RequestIR) []map[string]interface{} {
	out := []map[string]interface{}{}
	for _, msg := range ir.Messages {
		switch msg.Role {
		case "tool":
			out = append(out, map[string]interface{}{
				"type": "function_call_output", "call_id": msg.ToolCallID, "output": msg.Text,
			})
		case "assistant":
			if msg.Text != "" || len(msg.Parts) > 0 {
				content := []map[string]interface{}{}
				appended := false
				if len(msg.Parts) > 0 {
					for _, p := range msg.Parts {
						// v1.0.0（C7）：空 text 片段跳过，避免产出 {"type":"output_text","text":""}
						if p.Text != "" {
							content = append(content, map[string]interface{}{"type": "output_text", "text": p.Text})
							appended = true
						}
					}
				}
				if !appended {
					content = append(content, map[string]interface{}{"type": "output_text", "text": msg.Text})
				}
				out = append(out, map[string]interface{}{"role": "assistant", "content": content})
			}
			for _, c := range msg.ToolCalls {
				out = append(out, map[string]interface{}{
					"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": c.Args,
				})
			}
		case "system":
			// IR 系统词统一走 instructions，忽略
		default: // user
			if len(msg.Parts) > 0 {
				content := make([]map[string]interface{}, 0, len(msg.Parts))
				for _, p := range msg.Parts {
					if p.Type == "image" {
						content = append(content, map[string]interface{}{
							"type": "input_image", "image_url": imageDataURL(p),
						})
					} else if p.Text != "" { // v1.0.0（C7）：空 text 片段跳过
						content = append(content, map[string]interface{}{"type": "input_text", "text": p.Text})
					}
				}
				// v1.0.0（C7）：全被过滤时回退 Text，避免产出空 content 数组
				if len(content) > 0 {
					out = append(out, map[string]interface{}{"role": "user", "content": content})
				} else {
					out = append(out, map[string]interface{}{"role": "user", "content": msg.Text})
				}
			} else {
				out = append(out, map[string]interface{}{"role": "user", "content": msg.Text})
			}
		}
	}
	return out
}

func responsesToolChoiceValue(tc *ToolChoice) interface{} {
	if tc == nil {
		return "auto"
	}
	switch tc.Type {
	case "tool":
		return map[string]string{"type": "function", "name": tc.Name}
	case "any":
		return "required"
	case "none":
		return "none"
	default:
		return "auto"
	}
}

// ===== 响应解析（Responses → IR）=====

func parseResponsesResponse(body []byte) (*ResponseIR, error) {
	var resp struct {
		ID        string `json:"id"`
		CreatedAt int64  `json:"created_at"`
		Status    string `json:"status"`
		Model     string `json:"model"`
		Output    []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Status    string `json:"status"`
			Role      string `json:"role"`
			Name      string `json:"name"`
			CallID    string `json:"call_id"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			// v1.0.0（G4，关联检查报告 A4）：官方 reasoning 条目常仅以 summary 数组承载
			// 思考摘要（content 为显式开启输出模式后的新增通道），此前未解析导致静默丢失。
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("upstream error: %s", resp.Error.Message)
	}
	if resp.Status == "failed" {
		return nil, fmt.Errorf("upstream response status is failed")
	}
	ir := &ResponseIR{ID: resp.ID, Model: resp.Model, Created: resp.CreatedAt, Finish: "stop"}
	var texts []string
	var reasons []string
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" || c.Type == "text" {
					texts = append(texts, c.Text)
				}
			}
		case "reasoning":
			// v1.0.0（M6）：推理条目（content 内 reasoning_text/summary_text）归一入 Reasoning 通道
			got := 0
			for _, c := range item.Content {
				if c.Type == "reasoning_text" || c.Type == "summary_text" {
					reasons = append(reasons, c.Text)
					got++
				}
			}
			// v1.0.0（G4）：content 通道优先；本条目 content 无可命中条目时兜底读取
			// summary 通道（官方非流式响应 reasoning 的常见形态，仅 summary 数组）。
			// 判定按条目独立进行：前序条目已命中不影响本条目兜底。
			if got == 0 {
				for _, s := range item.Summary {
					if s.Type == "summary_text" || s.Type == "" {
						reasons = append(reasons, s.Text)
					}
				}
			}
		case "function_call":
			ir.ToolCalls = append(ir.ToolCalls, ToolCall{ID: item.CallID, Name: item.Name, Args: item.Arguments})
		}
	}
	ir.Text = strings.Join(texts, "")
	ir.Reasoning = strings.Join(reasons, "")
	if resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason == "max_output_tokens" {
		ir.Finish = "length"
	} else if len(ir.ToolCalls) > 0 {
		ir.Finish = "tool_calls"
	}
	if resp.Usage != nil {
		ir.PromptTokens = resp.Usage.InputTokens
		ir.CompletionTokens = resp.Usage.OutputTokens
		ir.TotalTokens = resp.Usage.TotalTokens
	}
	if ir.TotalTokens == 0 {
		ir.TotalTokens = ir.PromptTokens + ir.CompletionTokens
	}
	return ir, nil
}

// ===== 响应构建（IR → Responses）=====

func buildResponsesResponse(ir *ResponseIR, model string) []byte {
	if model == "" {
		model = ir.Model
	}
	created := ir.Created
	if created == 0 {
		created = nowUnix()
	}
	output := []map[string]interface{}{}
	// v1.0.0（M6）：思考内容前置 reasoning 条目（官方结构：summary 可空数组 + content 内 reasoning_text）
	if ir.Reasoning != "" {
		output = append(output, map[string]interface{}{
			"type": "reasoning", "id": "rs_llmproxy", "summary": []interface{}{},
			"content": []map[string]interface{}{{"type": "reasoning_text", "text": ir.Reasoning}},
			"status":  "completed",
		})
	}
	if ir.Text != "" || (len(ir.ToolCalls) == 0 && ir.Reasoning == "") {
		output = append(output, map[string]interface{}{
			"type":    "message",
			"id":      "msg_llmproxy",
			"status":  "completed",
			"role":    "assistant",
			"content": []map[string]interface{}{{"type": "output_text", "text": ir.Text, "annotations": []interface{}{}}},
		})
	}
	for i, c := range ir.ToolCalls {
		output = append(output, map[string]interface{}{
			"type":      "function_call",
			"id":        fmt.Sprintf("fc_%d", i+1),
			"call_id":   c.ID,
			"name":      c.Name,
			"arguments": c.Args,
			"status":    "completed",
		})
	}
	status := "completed"
	var incompleteDetails map[string]interface{}
	if ir.Finish == "length" {
		// 因 max_output_tokens 截断：status=incomplete + incomplete_details
		status = "incomplete"
		incompleteDetails = map[string]interface{}{"reason": "max_output_tokens"}
	}
	resp := map[string]interface{}{
		"id":         nonEmpty(ir.ID, "resp_llmproxy"),
		"object":     "response",
		"created_at": created,
		"status":     status,
		"model":      model,
		"output":     output,
		"usage": map[string]int64{
			"input_tokens":  ir.PromptTokens,
			"output_tokens": ir.CompletionTokens,
			"total_tokens":  ir.TotalTokens,
		},
		"output_text": ir.Text, // 便利字段（官方 SDK 由 output 计算，非 SDK 客户端可直接读取）
	}
	if incompleteDetails != nil {
		resp["incomplete_details"] = incompleteDetails
	}
	b, _ := json.Marshal(resp)
	return b
}
