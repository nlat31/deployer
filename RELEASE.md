# 独立发布与安装

构建机需要 Go 1.24+、Python 3、GNU tar、sha256sum；控制端额外需要 Node.js/npm。

```bash
./release-server.sh 0.1.7
./release-agent.sh 0.1.3
# GOARCH=arm64 ./release-agent.sh 0.1.3
```

输出在 `dist/`，每个 `.tar.gz` 旁有独立 `.sha256` 校验文件。压缩包内只有一个角色目录及以下文件：

| 控制端 | 客户端 |
| --- | --- |
| setup.sh、start.sh、stop.sh、remove.sh | setup.sh、start.sh、stop.sh、remove.sh |
| deployer-server | deployer-agent |

不附带文档、配置示例、内部清单、证书工具、源码、真实配置或数据。角色、架构及版本写在脚本注释中；配置默认值内嵌于 setup.sh。`deployerctl` 离线维护工具留在源码仓库，不随运行包分发。

```bash
sha256sum -c deployer-server-0.1.7-linux-amd64.tar.gz.sha256
tar -xzf deployer-server-0.1.7-linux-amd64.tar.gz
cd deployer-server-0.1.7-linux-amd64
sudo ./setup.sh
# 按需编辑 /data/deployer/config/server.env 并提供 mTLS 证书
sudo ./start.sh
sudo ./stop.sh
```

客户端同理，配置为 `/data/deployer/config/agent.env`。运行主机需要 Linux、systemd 249+、Bash、util-linux/flock 和 root 安装权限，不需要 Go、Python 或 Node.js。

`setup.sh` 检查架构和 systemd、创建服务帐号与目录、安装程序、生成缺失的环境配置、配置有上限的独立日志命名空间并启用开机自启。重复执行保留现有配置、数据库和证书；内容相同的程序不会重新替换。安装使用独占锁和原子替换。setup 不主动启动服务；先提供证书再执行 start。`start.sh` 对已运行的同一程序保持幂等，程序被升级后则重启该服务。`stop.sh` 只停止当前角色，可重复执行。

所有应用文件位于 `/data/deployer/`：`bin/` 保存程序，`config/` 保存配置和运行证书，`controller/` 保存控制端数据库、备份及包，`agent/` 保存客户端状态及应用。systemd 单元及 journald 配置使用系统标准目录。更新前备份数据与配置；不自动转换历史数据库结构。

## 证书

签发工具只留在源码仓库：

```bash
bash scripts/pki.sh init /safe/deployer-pki deployer.example.com 127.0.0.1
bash scripts/pki.sh node /safe/deployer-pki worker-01
```

`ca-key.pem` 只保留在安全签发机。将公开 `ca.pem` 放到配置目录，控制端接收 `server-cert.pem`、`server-key.pem`，客户端接收自己的 `client-cert.pem`、`client-key.pem`。控制端私钥属主 `deployer-control`、权限 0600；客户端私钥 root:root 0600。证书默认有效期十年，到期前换证，支持运行时加载新证书。

控制端按需配置 `DEPLOYER_LISTEN=0.0.0.0:7710`；客户端配置匹配证书 SAN 的 `DEPLOYER_SERVER=https://deployer.example.com:7710`。合法证书心跳自动登记，无需注册 token。初始帐号见源码 README，部署后在“我的帐号”改密并绑定 Google 验证码。

可选设置 `DEPLOYER_UI_LISTEN=127.0.0.1:7711`，使用 `ssh -L 17720:127.0.0.1:7711 loom` 访问回环控制页面。服务状态使用 `systemctl status deployer-server deployer-agent`；日志使用 `journalctl --namespace=deployer -u deployer-server`。

## 源码中的验收工具

`scripts/smoke-remote.py` 验证 mTLS、部署、切换、回滚、崩溃恢复、幂等请求、启停、数据清理、删除、重启和离线保留。仅用于明确指定的验收主机，不随运行包分发。

## 卸载

```bash
sudo ./remove.sh        # 发布包默认卸载本包角色；源码目录默认卸载两端
sudo ./remove.sh all    # 完全卸载控制端和客户端
```

卸载无需交互确认，会停止并禁用服务，删除对应程序、默认配置和数据、证书、服务帐号及 systemd 单元；卸载 Agent 时先停止其托管应用。另一角色仍安装时保留共享 CA 和日志，最后一个角色移除后清理整个 `/data/deployer` 和专用日志。可重复执行。操作不可恢复，请提前备份需要的数据。自定义到默认目录之外的数据路径不自动删除，下载的解压包和源码也保留。

## 公网 18.138.124.233 通用发布

Agent 的代码及初始配置默认连接 `https://18.138.124.233:7710`：

```bash
./release-public.sh 0.1.5
```

签发目录默认 `data/pki/loom`，可用 `DEPLOYER_RELEASE_PKI` 指定。控制端包监听 `0.0.0.0:7710`。同一个 Agent 包可直接分发到多台主机，无需指定节点 ID，也无需预签发客户端证书。各机解压后执行 `sudo ./setup.sh && sudo ./start.sh`。

通用 Agent 包只内嵌公开的服务端 CA。首次启动生成本机 P-256 私钥，原子保存后通过校验服务端证书的 HTTPS `/enroll` 发送签名 CSR；服务端忽略请求中的身份和扩展，按公钥 SHA256 摘要签发仅允许客户端认证的证书。私钥从不上传，也不随包分发。后续心跳、任务和包下载均强制 mTLS，只有合法心跳才保存主机。控制端使用自己的在线客户端 CA，原服务端 CA 私钥不上传；在线签发状态位于控制端 `state/agent-issuer/`，每日自动备份和 `deployerctl restore-backup` 一并保存、恢复该状态。

重复启动沿用身份；申请中断和网络故障重试不会生成新身份。客户端证书有效期最长十年，每小时检查并在到期前30天自动续期。在线客户端 CA 有效期二十年，到期前需轮换。`/enroll` 自动签发无需 token 或人工批准，可访问控制端且信任其 CA 的主机均可申请独立身份；每IP每分钟10次、全局每分钟1000次并限制请求大小。没有客户端证书的请求无法进入 Agent 业务接口，回环 HTTP 页面不提供注册入口。

控制端包仍包含自身 TLS 私钥，只部署到控制端；Agent 包没有任何私钥。首次启动时需能连公网 TCP 7710。不要复制已安装主机的 `client-key.pem`、`client-cert.pem` 或 Agent 数据目录来克隆主机。重复 setup 不覆盖已有配置和身份；旧安装更改连接地址需编辑 agent.env 并重启。

## 应用直接执行

当前 Agent 以 root 直接执行应用脚本，不创建应用 systemd 单元。仅控制端和 Agent 自身继续使用 systemd。Agent 服务采用 `KillMode=process`，服务重启保留应用；卸载执行 `--stop-apps` 停止应用后才删除运行记录。

更新旧执行器时，先通过旧 Agent 停止其运行中的应用并确认退出，再安装新 Agent，重新启动目标应用；新执行器不接管历史 systemd 应用服务。Loom 精简包需重新生成，包含与原版一致的 `setup-runtime.sh`，已有仓库版本不可原地替换。
