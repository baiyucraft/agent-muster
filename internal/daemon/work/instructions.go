package work

import (
	"fmt"
	"os"
	"path/filepath"
)

func collaborationInstructions() string {
	command := "muster"
	if executable, err := os.Executable(); err == nil {
		path := filepath.Join(filepath.Dir(executable), "muster.exe")
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			command = path
		}
	}
	return fmt.Sprintf(`

Agent Muster 为本轮提供协作工具。若能看到 muster MCP 工具，优先通过工具调用并使用其参数。也可使用本机命令 %q，以下旗标为命令行示例。
先调用 status，在 delegation_targets 里按项目、设备和 Agent 选择子任务目标。attachments 和 dependency_results 列出收到的文件及子任务状态。
使用 delegate -key 稳定键 -device 设备ID -binding 绑定ID -title 标题 -prompt 提示词 派出子任务。可用 -project 指定项目，-transfer 指定已发送的内容投递 ID（可重复）。
使用 send -key 稳定键 -device 设备ID -file 相对路径 发送工作目录中的文件，或用 -message 发送文本。
使用 receive -transfer 投递ID 取得本轮已接收文件的本机相对路径。
要在子任务结束或文件送达后继续当前会话，使用 wait -task 子任务ID 或 wait -transfer 投递ID（可重复），然后结束本轮回复以释放设备。`, command)
}
