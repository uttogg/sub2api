# 更新日志

## 未发布

### 新增功能

- 2026-10-13：新增管理员 OpenAI 额度 `POST /accounts/:id/quota/refresh-only` 接口，刷新额度及缓存时不主动通知自动重置任务；原有刷新接口保留通知行为、响应格式和缓存写入失败处理。

### 上游同步

- 合并 Sub2API v0.2.17 上游更新（2026-10-17；上游 main：b0a02a3304a0a78cdd003ddda1dd23ba02c03084）。
- 包含 v0.2.17 标签提交 `060d0160417782cd962a0c7f460f6cd4565d4916` 及标签后的 VERSION 同步提交；服务版本同步为 0.2.17。
- 同步 Gemini 4 Argon 模型与计费、兼容入口思考档位及摘要、Vertex 账号错误和共享容量重试、Antigravity 工具结果与模型目录、可配置最小输出 token、OpenAI 按工作区导入与模型停用判定、Messages web_search、Responses created_at、支付订单核验与退款保留扣减，以及账号筛选和支付页面修复。
- 按项目测试禁令保留本地测试、fixture、测试脚本、CI、根目录 Makefile 和 backend/.golangci.yml；未同步上游对应改动，未执行测试、编译或运行验证。账号连通性检查服务与页面属于生产功能，随上游同步。
- 合并 Sub2API v0.2.16 上游更新（2026-10-13；上游 main：4d9ac3e482106d2da3a39fe3e546d8d6801f1cc1）。
- 包含 v0.2.16 标签提交 `01842576b3d5c1e2545b5a99ad555d36e28ed306` 及标签后的 VERSION 同步提交；服务版本同步为 0.2.16。
- 同步 Gemini 显式上下文缓存、存储计费与迁移、Agent Platform (Vertex) API Key 及模型列表、分组多语言名称和描述、Claude Haiku 5.5、临时不可调度规则账号/模型作用范围、Responses 到 Chat 的安全回退，以及额度识别、监控和支付页面修复。
- 按项目测试禁令保留本地测试、fixture、测试脚本、CI、根目录 Makefile 测试列表及 backend/.golangci.yml；这些内容不在本次上游同步范围，另排除 accountNameMap.ts 中无生产消费者的测试专用 resetAccountNameCacheForTests 辅助函数。未执行测试、编译或运行验证。
- 2026-10-09：合并 Sub2API v0.2.15 上游更新（上游 main：3a6fd1c9db07203ca308aaba69e502bc1f35b307）。
- 同步 provider profile 与动态平台目录、Cline 和 Command Code 接入、协议路由与模型同步、Claude 计费及钱包冷却、OAuth web_search 历史声明、Responses refusal 与旧版 function_call 转换，以及账号管理、监控和支付页面修复。
- 按项目测试禁令保留本地测试、fixture、测试脚本及 CI 内容；上游对应文件、根目录 Makefile 测试列表和 backend/.golangci.yml 规则未同步，平台目录文件中仅供测试使用的两个清单替换辅助函数和 backend/internal/service/platform_catalog.go 测试生成辅助亦未同步。保留前端运行时使用的内置平台 JSON。未执行测试、编译或运行验证。

### 优化与修复

- 2026-10-17：整合上游与本地冲突，保留 OpenAI/Antigravity 混合调度及模型目录、DeepSeek 原生推理历史、WebSocket 本地语言规范化和协作错误反馈；将最小输出 token 配置接入本地转换选项，保留 Responses 流失败反馈并采用上游时间戳和事件序号。
- 2026-10-17：修复上游模型停用判定与 Antigravity 自映射形成的递归，保留混合分组中仍有有效替代账号的公共别名；补齐本地流错误反馈的时间戳参数，并保留账号行内筛选的五项无障碍名称。
- 2026-10-13：修正 Gemini 官方 API 地址的默认 HTTPS 端口识别，显式 `:443` 与未填写端口使用相同的官方缓存及额度处理，保持与前端 URL 规范化判定一致。
- 2026-10-13：将本地 `codex/0.2.15`（9b4b126ead179c9f6747a0e892458fcd3f2ce841）与上述上游 main 合并到本地 main；保留 Responses 推理历史、中断流失败反馈、混合调度和额度管理定制。解决生成的依赖注入接线冲突，同时保留 Antigravity 调度参数并接入 Gemini 缓存服务及停止回收流程。
- 2026-10-09：将本地 `codex/0.2.15`（562874fcd5f8ee912e715cd42e902f7f3223fccc）与上述上游 main 合并到本地 main；保留 native Responses reasoning replay、完整推理历史与中断流失败反馈，兼容上游旧版 function_call 配对和 refusal 内容处理。
- 2026-10-09：保留 OpenAI/Antigravity 混合调度快照及本地时区、额度重置和 OAuth 窗口激活功能；独立 amd64 构建文件的 Go 镜像默认版本同步为 1.27.2，与上游 go.mod 最低工具链要求保持一致。
- 2026-10-08：修复标准 Responses 流在上游读取异常、超限或超时后的终止反馈，复用 `response.failed` 并保留错误码及脱敏消息，补齐 `sequence_number` 与 `response.created_at`；保留输出前 failover 和既有计费语义。
