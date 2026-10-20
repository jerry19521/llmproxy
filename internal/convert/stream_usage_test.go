package convert

import (
	"bytes"
	"testing"
)

// TestParseOpenAIStreamUsageTotalOnly OpenAI 兼容流 usage 仅携带 total_tokens
// （个别网关的精简形态）时也应产生 evUsage 事件，否则跨格式终止帧的用量
// 统计静默丢失。
func TestParseOpenAIStreamUsageTotalOnly(t *testing.T) {
	evs := parseOpenAIStreamPayload(`{"id":"c1","usage":{"total_tokens":42}}`)
	found := false
	for _, ev := range evs {
		if ev.Kind == evUsage {
			found = true
			if ev.Total != 42 {
				t.Fatalf("evUsage.Total 应为 42，实际 %d", ev.Total)
			}
		}
	}
	if !found {
		t.Fatal("仅 total_tokens 的 usage 应产生 evUsage 事件")
	}
}

// TestSSEScannerDroppedBytesCounted 帧缓冲溢出保护丢弃的字节必须计数：
// 丢弃本身已不可恢复，再无观测通道会让故障上游/恶意代理的异常流完全不可见。
func TestSSEScannerDroppedBytesCounted(t *testing.T) {
	var sc sseScanner
	big := bytes.Repeat([]byte("x"), maxScannerBuffer+1024)
	sc.feed(big)
	want := int64(len(big) - maxScannerBuffer/2)
	if sc.dropped != want {
		t.Fatalf("溢出丢弃应计数 %d 字节，实际 %d", want, sc.dropped)
	}
}

// TestStreamConverterDroppedBytes 转换器应透出扫描器丢弃计数（供代理层告警）。
func TestStreamConverterDroppedBytes(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAI, FormatAnthropic, "m")
	conv.FeedChunk(bytes.Repeat([]byte("x"), maxScannerBuffer+1024))
	if conv.DroppedBytes() == 0 {
		t.Fatal("溢出丢弃应经 DroppedBytes 透出")
	}
}
