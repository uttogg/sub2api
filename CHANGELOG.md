# 更新日志

## 未发布

### 新增功能

- 2026-10-13：新增管理员 OpenAI 额度 `POST /accounts/:id/quota/refresh-only` 接口，刷新额度及缓存时不主动通知自动重置任务；原有刷新接口保留通知行为、响应格式和缓存写入失败处理。

### 上游同步

- 无。

### 优化与修复

- 2026-10-08：修复标准 Responses 流在上游读取异常、超限或超时后的终止反馈，复用 `response.failed` 并保留错误码及脱敏消息，补齐 `sequence_number` 与 `response.created_at`；保留输出前 failover 和既有计费语义。
