// sanitize-fixture 把一条真实消息 JSON（标准输入）脱敏成测试样例（标准输出）。
// 提交前仍要人工看一遍输出。
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/vinx-lab/vinx-assistant/internal/ilink"
)

func main() {
	raw, err := io.ReadAll(os.Stdin)
	if err == nil {
		raw, err = ilink.Sanitize(raw)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sanitize-fixture:", err)
		os.Exit(1)
	}
	os.Stdout.Write(raw)
}
