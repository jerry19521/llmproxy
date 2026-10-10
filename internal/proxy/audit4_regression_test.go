package proxy

// v1.8.5 复查修复的回归测试（对应需求 12 版本更新记录 v1.8.5 小节）：
//   G6  passthroughStream/convertStream 写失败块的部分写入字节计入 clientBytes（与 sentBytes 同口径）
//   G7  累积副本达 64MB 封顶后 content 检测覆盖当前块（first_content_ms 不再漏检）
//   G8  contentMarkers 排除 reasoning 阶段误命中（Responses reasoning_summary_text.done 含 "text" 字段）

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"llmproxy/internal/convert"
)

// ===== G8：contentMarkers 判定 =====

// TestStreamChunkHasContentMarkers G8 回归：marker 集合对三种格式的 content 帧
// 正常命中，对 reasoning 阶段帧（含曾误命中的 reasoning_summary_text.done）不命中。
// 修复前裸 `"text":` marker 会把 reasoning_summary_text.done 误判为 content 起点，
// first_content_ms 偏早。
func TestStreamChunkHasContentMarkers(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"OpenAI delta.content", `data: {"choices":[{"delta":{"content":"hi"}}]}`, true},
		{"OpenAI 开场 role 帧（content 空串，预期命中）", `data: {"choices":[{"delta":{"role":"assistant","content":""}}]}`, true},
		{"Anthropic text_delta", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}", true},
		{"Anthropic content_block_start(text)", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}", true},
		// Responses 真实流：首个文本输出前必有 content_part.added（part.type=output_text，
		// 与 marker `"output_text"` 对齐）先行到达；output_text.delta 事件名为
		// "response.output_text.delta"（output_text 前是点号非引号，单帧不命中 marker）
		{"Responses content_part.added（part.type=output_text）", "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]}}", true},
		{"Responses 完整输出序列（added→part.added→delta）", "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]}}\n\nevent: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\"hello\"}}", true},
		// G8 核心回归：修复前 `"text":` marker 误命中（reasoning 阶段被误判为 content 起点）
		{"Responses reasoning_summary_text.done（含 text 字段，须排除）", "event: response.reasoning_summary_text.done\ndata: {\"type\":\"response.reasoning_summary_text.done\",\"item_id\":\"rs_1\",\"summary_index\":0,\"text\":\"思考摘要完成\"}", false},
		{"Anthropic thinking_delta", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"思\"}}", false},
		{"OpenAI reasoning_content", `data: {"choices":[{"delta":{"reasoning_content":"思"}}]}`, false},
		{"Responses reasoning_text.delta", "event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"思\"}", false},
		{"空数据", "", false},
	}
	for _, c := range cases {
		if got := streamChunkHasContent([]byte(c.data)); got != c.want {
			t.Errorf("G8：%s 判定=%v，应为 %v", c.name, got, c.want)
		}
	}
}

// ===== G7：副本封顶后的 content 检测 =====

// audit4CapReader 填充块复用同一底层切片（不真实分配多份 64KB），提供恰好
// 填满 maxUpstreamResponseBytes 的块序列 + 一块含 content 帧的尾块。
type audit4CapReader struct {
	blocks int // 剩余填充块数
	fill   []byte
	tail   []byte
	done   bool
}

func (r *audit4CapReader) Read(p []byte) (int, error) {
	if r.blocks > 0 {
		r.blocks--
		return copy(p, r.fill), nil
	}
	if !r.done {
		r.done = true
		return copy(p, r.tail), nil
	}
	return 0, io.EOF
}
func (r *audit4CapReader) Close() error { return nil }

// audit4DiscardWriter 丢弃全部写入（64MB 压测下避免再积累一份客户端副本）。
type audit4DiscardWriter struct{ header http.Header }

func (w *audit4DiscardWriter) Header() http.Header         { return w.header }
func (w *audit4DiscardWriter) WriteHeader(int)             {}
func (w *audit4DiscardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestPassthroughStreamContentDetectAfterCap G7 回归：累积副本达 64MB 封顶后
// data 不再增长，仅扫 data 的旧检测对后续块中的 content 帧永远漏检（first_content_ms
// 恒 -1）；修复后检测同时覆盖当前块。
func TestPassthroughStreamContentDetectAfterCap(t *testing.T) {
	fill := bytes.Repeat([]byte("x"), 64*1024) // 无任何 marker 的纯填充
	blocks := int(maxUpstreamResponseBytes) / len(fill)
	if blocks*len(fill) != int(maxUpstreamResponseBytes) {
		t.Fatalf("G7：填充块长度应整除封顶值（%d %% %d != 0）", int(maxUpstreamResponseBytes), len(fill))
	}
	tail := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &audit4CapReader{blocks: blocks, fill: fill, tail: tail},
	}
	att := &streamAttempt{result: &upstreamResult{}}
	passthroughStream(&audit4DiscardWriter{header: http.Header{}}, resp, att, time.Now())

	if att.result.ReadChunks != int64(blocks+1) {
		t.Fatalf("G7：应读取 %d 块，实际 %d", blocks+1, att.result.ReadChunks)
	}
	if att.firstContentMs < 0 {
		t.Fatalf("G7：副本封顶后当前块含 content 帧时 firstContent 应记录，实际未检出（仅扫已封顶的累积副本，修复前恒漏检）")
	}
}

// ===== G6：写失败块的部分写入口径 =====

// audit4PartialWriter 第 failAt 次 Write 起返回 partial 字节 + 错误
// （模拟 w.Write 部分送达后连接中断：audit3FailWriter 只覆盖 wn=0 场景）。
type audit4PartialWriter struct {
	header      http.Header
	buf         bytes.Buffer
	writes      int
	failAt      int
	partial     int
	err         error
	failPayload []byte // 失败那次收到的载荷副本（校验部分写入内容用）
}

func (w *audit4PartialWriter) Header() http.Header { return w.header }
func (w *audit4PartialWriter) WriteHeader(int)     {}
func (w *audit4PartialWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes >= w.failAt {
		w.failPayload = append([]byte{}, p...)
		return w.partial, w.err
	}
	w.buf.Write(p)
	return len(p), nil
}

// TestPassthroughStreamPartialWriteAccounting G6 回归（同格式透传路径）：写失败块的
// 部分写入字节计入 clientBytes。修复前 sentBytes 含部分写入 wn 而 clientBytes 截断到
// 上一块（sent > client 口径矛盾），且 RespBody 上游副本可能被原地 append 破坏。
func TestPassthroughStreamPartialWriteAccounting(t *testing.T) {
	part1 := bytes.Repeat([]byte("A"), 32)
	part2 := bytes.Repeat([]byte("B"), 100)
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(&audit3SeqReader{parts: [][]byte{part1, part2}}),
	}
	w := &audit4PartialWriter{header: http.Header{}, failAt: 2, partial: 37, err: errors.New("wsasend: partial")}
	att := &streamAttempt{result: &upstreamResult{}}
	passthroughStream(w, resp, att, time.Now())

	if att.err == "" || !strings.Contains(att.err, "写入客户端失败") {
		t.Fatalf("G6：应记录写失败，实际 err=%q", att.err)
	}
	wantSent := int64(len(part1) + 37)
	if att.sentBytes != wantSent {
		t.Fatalf("G6：sent_bytes 应为 %d（含部分写入 37），实际 %d", wantSent, att.sentBytes)
	}
	if int64(len(att.clientBytes)) != wantSent {
		t.Fatalf("G6：clientBytes 应 %d 字节与 sent_bytes 同口径，实际 %d 字节——修复前部分写入被丢弃、client < sent", wantSent, len(att.clientBytes))
	}
	if !bytes.Equal(att.clientBytes[:len(part1)], part1) {
		t.Fatalf("G6：clientBytes 前缀应为第一块")
	}
	if !bytes.Equal(att.clientBytes[len(part1):], part2[:37]) {
		t.Fatalf("G6：clientBytes 尾部应为第二块前 37 字节（部分写入），实际 %q", att.clientBytes[len(part1):])
	}
	// 上游副本保留完整读取字节（部分写入拼接不得污染 RespBody 底层数组）
	if !bytes.Equal(att.result.RespBody, append(append([]byte{}, part1...), part2...)) {
		t.Fatalf("G6：resp_body 应保留完整上游字节（132 字节），实际 %d 字节", len(att.result.RespBody))
	}
}

// TestConvertStreamPartialWriteAccounting G6 回归（跨格式转换路径）：writeOut 失败块的
// 部分写入字节计入 clientBytes。修复前 writeOut 失败时整块丢弃（sentBytes 含 wn 而
// clientBytes 不含，口径矛盾）。
func TestConvertStreamPartialWriteAccounting(t *testing.T) {
	frame1 := []byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"AB\"},\"finish_reason\":null}]}\n\n")
	frame2 := []byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"CD\"},\"finish_reason\":null}]}\n\n")
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(&audit3SeqReader{parts: [][]byte{frame1, frame2}}),
	}
	const partial = 21
	w := &audit4PartialWriter{header: http.Header{}, failAt: 2, partial: partial, err: errors.New("wsasend: partial")}
	att := &streamAttempt{result: &upstreamResult{}}
	convertStream(w, resp, att, time.Now(), convert.FormatOpenAI, convert.FormatResponses, "m")

	if att.err == "" || !strings.Contains(att.err, "写入客户端失败") {
		t.Fatalf("G6：应记录写失败，实际 err=%q", att.err)
	}
	if w.writes != 2 {
		t.Fatalf("G6：应恰两次 Write（第一帧成功、第二帧部分失败），实际 %d——若转换器对首帧未产生输出请调整用例帧", w.writes)
	}
	wantSent := int64(w.buf.Len() + partial)
	if att.sentBytes != wantSent {
		t.Fatalf("G6：sent_bytes 应为 %d（成功 %d + 部分写入 %d），实际 %d", wantSent, w.buf.Len(), partial, att.sentBytes)
	}
	if int64(len(att.clientBytes)) != wantSent {
		t.Fatalf("G6：跨格式路径 clientBytes 应 %d 字节与 sent_bytes 同口径，实际 %d 字节——修复前失败块整体丢弃", wantSent, len(att.clientBytes))
	}
	if !bytes.HasPrefix(att.clientBytes, w.buf.Bytes()) {
		t.Fatalf("G6：clientBytes 前缀应为成功写出的转换输出")
	}
	if !bytes.Equal(att.clientBytes[w.buf.Len():], w.failPayload[:partial]) {
		t.Fatalf("G6：clientBytes 尾部应为失败块前 %d 字节（部分写入），实际 %q", partial, att.clientBytes[w.buf.Len():])
	}
}
