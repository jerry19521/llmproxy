// sse.go：完整响应 → SSE 事件流的一次性合成。
//
// v1.0.0 起跨格式流式为边收边转发（stream.go 的 StreamConverter 实时转换），
// 不再强制上游非流式；本文件仅服务「上游忽略 stream 参数、对 stream=true
// 仍返回完整 JSON」的兼容网关兜底场景（代理层 fallbackFullBodySSE 调用）：
// 将非流式响应解析为 IR 后，一次性合成为客户端格式的完整 SSE 事件序列返回。
package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// BuildSSE 将完整 JSON 响应（客户端格式）合成为该格式的 SSE 事件流。
func BuildSSE(format string, respBody []byte) ([]byte, error) {
	ir, err := ParseResponse(format, respBody)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	switch NormalizeAPIType(format) {
	case FormatAnthropic:
		anthropicSSE(&buf, ir)
	case FormatResponses:
		responsesSSE(&buf, ir)
	default:
		openAISSE(&buf, ir)
	}
	return buf.Bytes(), nil
}

// sseFrame 写入一个 SSE 帧（event 行可选）。
func sseFrame(buf *bytes.Buffer, event string, data interface{}) {
	b, _ := json.Marshal(data)
	if event != "" {
		fmt.Fprintf(buf, "event: %s\n", event)
	}
	fmt.Fprintf(buf, "data: %s\n\n", b)
}

// ===== OpenAI Chat Completions SSE =====

func openAISSE(buf *bytes.Buffer, ir *ResponseIR) {
	id := nonEmpty(ir.ID, "chatcmpl-llmproxy")
	created := ir.Created
	if created == 0 {
		created = nowUnix()
	}
	model := nonEmpty(ir.Model, "gpt")
	chunk := func(delta map[string]interface{}, finish interface{}, withUsage bool) {
		choice := map[string]interface{}{"index": 0, "delta": delta, "finish_reason": finish}
		m := map[string]interface{}{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []map[string]interface{}{choice},
		}
		if withUsage {
			m["usage"] = map[string]int64{
				"prompt_tokens":     ir.PromptTokens,
				"completion_tokens": ir.CompletionTokens,
				"total_tokens":      ir.TotalTokens,
			}
		}
		sseFrame(buf, "", m)
	}
	chunk(map[string]interface{}{"role": "assistant", "content": ""}, nil, false)
	// v1.0.0（M6）：思考内容前置 reasoning_content chunk
	if ir.Reasoning != "" {
		chunk(map[string]interface{}{"reasoning_content": ir.Reasoning}, nil, false)
	}
	if ir.Text != "" {
		chunk(map[string]interface{}{"content": ir.Text}, nil, false)
	}
	for i, c := range ir.ToolCalls {
		chunk(map[string]interface{}{
			"tool_calls": []map[string]interface{}{{
				"index":    i,
				"id":       c.ID,
				"type":     "function",
				"function": map[string]string{"name": c.Name, "arguments": c.Args},
			}},
		}, nil, false)
	}
	finish := nonEmpty(ir.Finish, "stop")
	chunk(map[string]interface{}{}, finish, true)
	buf.WriteString("data: [DONE]\n\n")
}

// ===== Anthropic Messages SSE =====

func anthropicSSE(buf *bytes.Buffer, ir *ResponseIR) {
	id := nonEmpty(ir.ID, "msg_llmproxy")
	model := nonEmpty(ir.Model, "claude")
	stopReason := finishToAnthropic(nonEmpty(ir.Finish, "stop"))
	sseFrame(buf, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int64{"input_tokens": ir.PromptTokens, "output_tokens": 0},
		},
	})
	idx := 0
	// v1.0.0（M6）：思考内容前置 thinking 块（signature 空置，展示用途；回传历史时请求解析器忽略）
	if ir.Reasoning != "" {
		sseFrame(buf, "content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
		})
		sseFrame(buf, "content_block_delta", map[string]interface{}{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]interface{}{"type": "thinking_delta", "thinking": ir.Reasoning},
		})
		sseFrame(buf, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
		idx++
	}
	if ir.Text != "" || len(ir.ToolCalls) == 0 {
		sseFrame(buf, "content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
		if ir.Text != "" {
			sseFrame(buf, "content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": idx,
				"delta": map[string]interface{}{"type": "text_delta", "text": ir.Text},
			})
		}
		sseFrame(buf, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
		idx++
	}
	for _, c := range ir.ToolCalls {
		args := c.Args
		if args == "" {
			args = "{}" // v1.0.0（M5）：无参工具参数为空串时以 "{}" 输出，避免客户端聚合 input 为空串解析失败
		}
		sseFrame(buf, "content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]interface{}{"type": "tool_use", "id": c.ID, "name": c.Name, "input": map[string]interface{}{}},
		})
		sseFrame(buf, "content_block_delta", map[string]interface{}{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": args},
		})
		sseFrame(buf, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
		idx++
	}
	sseFrame(buf, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]int64{"output_tokens": ir.CompletionTokens},
	})
	sseFrame(buf, "message_stop", map[string]interface{}{"type": "message_stop"})
}

// ===== OpenAI Responses SSE =====

func responsesSSE(buf *bytes.Buffer, ir *ResponseIR) {
	model := nonEmpty(ir.Model, "gpt")
	created := ir.Created
	if created == 0 {
		created = nowUnix()
	}
	finish := nonEmpty(ir.Finish, "stop")
	usage := map[string]int64{
		"input_tokens":  ir.PromptTokens,
		"output_tokens": ir.CompletionTokens,
		"total_tokens":  ir.TotalTokens,
	}
	outputIndex := 0
	outputItems := []map[string]interface{}{}
	// v1.0.0（G3，关联检查报告 A3）：created/in_progress 前置于所有 output_item 事件，
	// 对齐官方事件序列（首事件必须为 response.created，此前兜底合成路径 reasoning 条目先于 created 写入）
	respSkeleton := func(status string) map[string]interface{} {
		return map[string]interface{}{
			"id": nonEmpty(ir.ID, "resp_llmproxy"), "object": "response", "created_at": created,
			"status": status, "model": model, "output": []interface{}{},
		}
	}
	sseFrame(buf, "response.created", map[string]interface{}{
		"type": "response.created", "response": respSkeleton("in_progress"),
	})
	sseFrame(buf, "response.in_progress", map[string]interface{}{
		"type": "response.in_progress", "response": respSkeleton("in_progress"),
	})
	// v1.0.0（M6）：思考内容前置 reasoning 条目（事件结构对齐官方）
	if ir.Reasoning != "" {
		reasoningItem := map[string]interface{}{
			"type": "reasoning", "id": "rs_llmproxy", "summary": []interface{}{},
			"content": []map[string]interface{}{{"type": "reasoning_text", "text": ir.Reasoning}},
			"status":  "completed",
		}
		outputItems = append(outputItems, reasoningItem)
		sseFrame(buf, "response.output_item.added", map[string]interface{}{
			"type": "response.output_item.added", "output_index": outputIndex,
			"item": map[string]interface{}{
				"type": "reasoning", "id": "rs_llmproxy", "summary": []interface{}{}, "status": "in_progress",
			},
		})
		sseFrame(buf, "response.reasoning_text.delta", map[string]interface{}{
			"type": "response.reasoning_text.delta", "item_id": "rs_llmproxy",
			"output_index": outputIndex, "content_index": 0, "delta": ir.Reasoning,
		})
		sseFrame(buf, "response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": outputIndex, "item": reasoningItem,
		})
		outputIndex++
	}
	// message 条目
	msgItem := map[string]interface{}{
		"type": "message", "id": "msg_llmproxy", "status": "completed", "role": "assistant",
		"content": []map[string]interface{}{{"type": "output_text", "text": ir.Text, "annotations": []interface{}{}}},
	}
	emitTextItem := ir.Text != "" || len(ir.ToolCalls) == 0
	if emitTextItem {
		outputItems = append(outputItems, msgItem)
		sseFrame(buf, "response.output_item.added", map[string]interface{}{
			"type": "response.output_item.added", "output_index": outputIndex,
			"item": map[string]interface{}{
				"type": "message", "id": "msg_llmproxy", "status": "in_progress", "role": "assistant",
				"content": []interface{}{},
			},
		})
		sseFrame(buf, "response.content_part.added", map[string]interface{}{
			"type": "response.content_part.added", "item_id": "msg_llmproxy",
			"output_index": outputIndex, "content_index": 0,
			"part": map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
		})
		if ir.Text != "" {
			sseFrame(buf, "response.output_text.delta", map[string]interface{}{
				"type": "response.output_text.delta", "item_id": "msg_llmproxy",
				"output_index": outputIndex, "content_index": 0, "delta": ir.Text,
			})
		}
		sseFrame(buf, "response.output_text.done", map[string]interface{}{
			"type": "response.output_text.done", "item_id": "msg_llmproxy",
			"output_index": outputIndex, "content_index": 0, "text": ir.Text,
		})
		sseFrame(buf, "response.content_part.done", map[string]interface{}{
			"type": "response.content_part.done", "item_id": "msg_llmproxy",
			"output_index": outputIndex, "content_index": 0,
			"part": map[string]interface{}{"type": "output_text", "text": ir.Text, "annotations": []interface{}{}},
		})
		sseFrame(buf, "response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": outputIndex, "item": msgItem,
		})
		outputIndex++
	}
	for i, c := range ir.ToolCalls {
		fcID := fmt.Sprintf("fc_%d", i+1)
		args := c.Args
		if args == "" {
			args = "{}" // v1.0.0（M5）：无参工具参数为空串时以 "{}" 输出
		}
		fcItem := map[string]interface{}{
			"type": "function_call", "id": fcID, "call_id": c.ID,
			"name": c.Name, "arguments": args, "status": "completed",
		}
		outputItems = append(outputItems, fcItem)
		sseFrame(buf, "response.output_item.added", map[string]interface{}{
			"type": "response.output_item.added", "output_index": outputIndex,
			"item": map[string]interface{}{
				"type": "function_call", "id": fcID, "call_id": c.ID,
				"name": c.Name, "arguments": "", "status": "in_progress",
			},
		})
		sseFrame(buf, "response.function_call_arguments.delta", map[string]interface{}{
			"type": "response.function_call_arguments.delta", "item_id": fcID,
			"output_index": outputIndex, "delta": args,
		})
		sseFrame(buf, "response.function_call_arguments.done", map[string]interface{}{
			"type": "response.function_call_arguments.done", "item_id": fcID,
			"output_index": outputIndex, "arguments": args,
		})
		sseFrame(buf, "response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": outputIndex, "item": fcItem,
		})
		outputIndex++
	}
	// 终态事件：因 max_output_tokens 截断时为 response.incomplete，其余 completed
	finalEvent := "response.completed"
	finalResp := map[string]interface{}{
		"id": nonEmpty(ir.ID, "resp_llmproxy"), "object": "response", "created_at": created,
		"status": "completed", "model": model, "output": outputItems,
		"usage": usage, "output_text": ir.Text,
	}
	if finish == "length" {
		finalEvent = "response.incomplete"
		finalResp["status"] = "incomplete"
		finalResp["incomplete_details"] = map[string]string{"reason": "max_output_tokens"}
	}
	sseFrame(buf, finalEvent, map[string]interface{}{
		"type": finalEvent, "response": finalResp,
	})
}
