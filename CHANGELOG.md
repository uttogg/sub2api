# 更新日志

## 未发布

### 新增功能

- 无。

### 上游同步

- 无。

### 优化与修复

- 2026-10-08：修复标准 Responses 流在上游读取异常、超限或超时后的终止反馈，复用 `response.failed` 并保留错误码及脱敏消息，补齐 `sequence_number` 与 `response.created_at`；保留输出前 failover 和既有计费语义。
