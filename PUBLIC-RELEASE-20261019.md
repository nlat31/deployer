# 公网正式发布与控制台验收（2026-10-19）

目标地址：18.138.124.233，SSH 别名 loom。正式版本为 Loom 0.8.14、Deployer 0.2.0，Linux amd64。

## 已通过的现场验收

使用真实公网控制台完成签名版本上传和两节点批量更新。本机 lenovo 与独立临时 root Agent 从 rc.3 更新至 rc.4；首台提交后暂停批次，第二台仍保持 rc.3；恢复后两台分别完成 60 秒试运行和 5 分钟稳定观察，批次 index 达到 2。

通过控制台上传签名有效但 exit 43 的候选程序，仅选择本机 Agent。候选版失败后自动恢复 rc.4，批次暂停，失败原因记录为 `trial exited: exit status 43`。真实 Loom Worker 的进程身份在更新及回退期间保持不变。

在两台客户端部署独立测试应用 console-cleanup-probe：

- 从控制台删除运行中的版本 2，进程退出、版本目录删除；版本 1 的数据仍存在。
- 从控制台删除整个应用，客户端先停止进程再删除整个应用目录。临时 Agent 暂停连接时，其目录仍保留、主控状态为 pending；重新连接后清理完成。
- 确认两个客户端应用目录均不存在，进程均退出，主控应用记录及安装清单均不再包含目标。

机器证据位于 `.cache/console-public-20261019/`：ui-report.json、delete-report.json、offline-report.json；控制台截图 live-agent-updates.png、delete-2.png、delete-app.png。验收脚本临时出现的 PATH、上传等待和重复打包时间戳问题已修正；上述通过结论来自修正后实际完成的流程。

## 正式发行包

两个项目的 dist 已在现场测试通过后清空并重新构建，无 rc 包残留。版本文件 VERSION、程序默认版本和前端元数据均调整为正式版本。

Loom dist 包含正式服务端、工具依赖包，以及独立/Deployer 托管两种方式各自的 slim/full Worker，共 6 个归档。Deployer dist 包含正式服务端和 Agent 两个归档，另附 Agent 程序及签名清单。所有归档附 SHA-256，共 8 个包，校验记录为 formal-packages.json。

完整 Worker 包使用从实际公网 Worker 缓存重建的公开签名组件，按原 manifest 列表复制，无客户端私钥。打包再次校验组件签名和文件摘要；没有替换公网签名密钥或绕过校验。slim 包首启从主控下载组件。

正式构建执行了 Loom 全量 Go、16 项组件、7 项发行和 10 项主控脚本测试；Deployer 发布工具、签名和 launcher 测试、正式 Agent 临时根目录幂等部署通过。此前全量 race 与浏览器测试记录见 RELIABILITY.md。

## 最终部署状态

正式服务端已替换并启动，Loom 26 条历史任务、8 条组件记录及 Deployer 原数据库文件均已核对保留。首次部署校验因重启后复用旧登录会话触发恢复旧程序；修正校验脚本重新登录后，正式替换成功，期间未恢复或重建数据库。最终以 `.cache/console-public-20261019/final-local.json`、formal-ui-report.json 和远端 `/data/releases/formal-20261019/deployment.json` 为准。正式替换使用 `/data/backups/formal-20261019` 保存一致性备份，保留数据库、配置、证书及历史业务记录，清除旧运行程序。

新版 Worker 的 App 标识及显示名称均为 loom-worker，版本 0.8.14，HTTP 健康检查为本机 18887/readyz、启动截止 300 秒。只上传仓库，等待用户部署，不自动下发新版本。


## 最终核验结果

- 公网 Loom 服务端 0.8.14、Deployer 服务端 0.2.0 均以 root 运行，HTTPS 就绪检查通过。
- 本机 Agent 已从控制台提交至正式 0.2.0，EUID 为 0，更新批次暂停。保留原有 16 台其他客户端记录，临时验收节点已删除，最终共 17 台节点。
- 本机旧 loom-worker 部署已通过停止并删除流程清理，`/data/deployer/agent/apps/` 无应用目录；临时 Agent 整个安装目录已删除，旧 Agent 运行版本只保留正式当前程序。
- App 标识及显示名称均为 loom-worker，正式 0.8.14 的 slim 托管包已入库为 available，SHA-256 为 `2ea8c31d1f09b0281167e2fe8e2fe4dce797a86ee8db644f7598d8b2ec1610e6`。没有节点部署或期望部署该版本，等待用户操作。
- 服务器旧运行版本及旧发布暂存目录已删除，保留数据库、配置和备份。备份恢复工具保存在 `/data/releases/formal-20261019/recovery.py`，数据库备份为 `/data/backups/formal-20261019`。

本次数据库压缩备份期间，控制端停机超过旧 Worker 的健康窗口，该 Worker 曾由既有健康恢复策略重启；这与此前 rc.4 批量更新/失败回退期间保持业务进程身份不变的验收是不同阶段。正式 Agent 更新时，运行中的 Worker 进程早于该 Agent 的试运行；最后已按旧版本清理要求停止并移除。

最终机器证据：final-local.json、final-public.json、formal-ui-report.json、remote-deployment.json、remote-cleanup.json、formal-packages.json。截图 formal-worker-ready.png 展示正式版本已入库；live-failure-reason.png 展示真实崩溃候选的控制台失败原因。
