package codexreg

import (
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"strconv"

	http "github.com/bogdanfinn/fhttp"
)

// setDatadogTraceHeaders 复现页面里 Datadog RUM SDK 给同源 XHR 加的追踪头。
// 真实抓包中 x-datadog-trace-id / x-datadog-parent-id 的十进制值分别等于
// traceparent 里 trace-id 低 64 位与 span-id 的十六进制值，这里保持同样的对应关系。
func setDatadogTraceHeaders(h http.Header) {
	trace := randUint63()
	span := randUint63()
	h.Set("x-datadog-origin", "rum")
	h.Set("x-datadog-parent-id", strconv.FormatUint(span, 10))
	h.Set("x-datadog-sampling-priority", "1")
	h.Set("x-datadog-trace-id", strconv.FormatUint(trace, 10))
	h.Set("traceparent", fmt.Sprintf("00-%016x%016x-%016x-01", uint64(0), trace, span))
	h.Set("tracestate", "dd=s:1;o:rum")
}

func randUint63() uint64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return 1<<62 + 12345
	}
	v := binary.BigEndian.Uint64(b[:]) & ((1 << 63) - 1)
	if v == 0 {
		v = 1
	}
	return v
}
