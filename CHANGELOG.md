# 更新日志

## 未发布

### 新增功能

- 无。

### 上游同步

- 2026-10-09：合并 Sub2API v0.2.15 上游更新（上游 main：3a6fd1c9db07203ca308aaba69e502bc1f35b307）。
- 同步 provider profile 与动态平台目录、Cline 和 Command Code 接入、协议路由与模型同步、Claude 计费及钱包冷却、OAuth web_search 历史声明、Responses refusal 与旧版 function_call 转换，以及账号管理、监控和支付页面修复。
- 按项目测试禁令保留本地测试、fixture、测试脚本及 CI 内容；上游对应文件、根目录 Makefile 测试列表和 backend/.golangci.yml 规则未同步，平台目录文件中仅供测试使用的两个清单替换辅助函数和 backend/internal/service/platform_catalog.go 测试生成辅助亦未同步。保留前端运行时使用的内置平台 JSON。未执行测试、编译或运行验证。

### 优化与修复

- 2026-10-09：将本地 `codex/0.2.15`（562874fcd5f8ee912e715cd42e902f7f3223fccc）与上述上游 main 合并到本地 main；保留 native Responses reasoning replay、完整推理历史与中断流失败反馈，兼容上游旧版 function_call 配对和 refusal 内容处理。
- 2026-10-09：保留 OpenAI/Antigravity 混合调度快照及本地时区、额度重置和 OAuth 窗口激活功能；独立 amd64 构建文件的 Go 镜像默认版本同步为 1.27.2，与上游 go.mod 最低工具链要求保持一致。
- 2026-10-08：修复标准 Responses 流在上游读取异常、超限或超时后的终止反馈，复用 `response.failed` 并保留错误码及脱敏消息，补齐 `sequence_number` 与 `response.created_at`；保留输出前 failover 和既有计费语义。
