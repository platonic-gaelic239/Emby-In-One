# Emby-In-One

> **Version: V1.3**

[更新日志](Update.md) | [English README](README_EN.md) | [安全策略](SECURITY.md) | [更新计划](Update%20Plan.md) | [V1.2.1 旧版文档](README_V1.2.1.md) | [GitHub](https://github.com/ArizeSky/Emby-In-One)

多台 Emby 服务器聚合代理，将多个上游 Emby 服务器的媒体库合并为一个统一入口，支持任何标准 Emby 客户端访问。（本版本为 V1.3 Go 后端重构 Pre-release 版，具备更高性能与并发能力）

## 测试站点

[演示站点](https://emby.cothx.eu.cc/)
Emby连接地址：https://emby.cothx.eu.cc/
账号：admin
密码：5T5xF4oMxcnrcCPA

## 预览

![预览图1](https://cdn.nodeimage.com/i/D293pIQcFNx4gXkfskPbnXFzmgCQ1JPx.webp)
![预览图2](https://cdn.nodeimage.com/i/iDAXrYaIXdm9efhwl2BtqJjRUmGfTSKU.webp)
![预览图3](https://cdn.nodeimage.com/i/K4jhTTMjv8rkHYiPNbXKUC0kXIzAXgq0.webp)
![预览图4](https://cdn.nodeimage.com/i/50eO6lJBev4Q5Zb1XhPgVH78kELtR1YK.webp)

> 图床服务由 [NodeImage](https://www.nodeimage.com) 提供，感谢支持。

---

## 功能概览

- **多服务器聚合** — 将多台服务器的影视库、搜索结果合并展示。采用 Goroutine 并发请求，聚合延迟仅取决于最慢的一台服务器。
- **智能去重与优选** — 相同影片自动合并，保留多版本源可选；支持 4 级元数据优先级（指定标记 > 中文 > 长度 > 顺序）智能摘取最佳展示信息。
- **客户端透传 (Passthrough)** — 按 Token 隔离透传客户端身份至上游（防串号），独创 5 级验证链持久化保存客户端特征，支持断线重连。
- **高阶 UA 伪装** — 支持 Infuse 伪装，亦可通过 `custom` 模式为每台上游独立自定义全部 5 个 Emby 客户端标识字段。
- **网络代理池** — 各个上游服务器可独立配置专属 HTTP/HTTPS 代理，内置一键连通性测试。
- **双播放模式** — 代理模式（流量中转，隐藏上游，支持 HLS/分段）或 直连模式（302 跳转上游，不耗费代理机流量）。
- **完全管控与运维** — 内置现代化 SSH 命令行管理菜单及 Web 管理面板；自带持久化日志与 SQLite ID 映射。
- **安全加固** — 登录防暴力破解（锁定IP）、配置文件 0600 安全权限原子写入、请求体超限防护、防并发冲突与优雅关机。

---

## 快速安装

> **旧版 Node.js 部署说明**：如果您希望部署基于 Node.js 的 V1.2.1 稳定版，请前往本仓库的 [Releases 页面](https://github.com/ArizeSky/Emby-In-One/releases) 下载 V1.2.1 的 Source code 源码压缩包，解压后同样运行 `bash install.sh` 即可。

本项目优先推荐在 Linux 操作系统使用 Docker 方式部署 V1.3 (Go 后端) 本地版。

### 方式一：一键安装脚本（推荐）

```bash
git clone https://github.com/ArizeSky/Emby-In-One.git
cd Emby-In-One
bash install.sh
```

脚本将为您自动安装 Docker 环境、分配随机管理员密码、构建 Go 版镜像并启动服务。后续如需管理，通过 SSH 输入 `emby-in-one` 即可呼出管理菜单。

### 方式二：手动 Docker Compose 部署

1. 创建项目目录：
```bash
mkdir -p /opt/emby-in-one/{config,data}
cd /opt/emby-in-one
```
2. 拷贝本仓库下的所有核心文件（包括 `go.mod`, `cmd/`, `internal/`, `public/`, `Dockerfile`, `docker-compose.yml` 等）至该目录。
3. 创建初始配置文件 `config/config.yaml`：
```yaml
server:
  port: 8096
  name: "Emby-In-One"

admin:
  username: "admin"
  password: "your-strong-password" # 首次启动后自动加密存储

playback:
  mode: "proxy"

timeouts:
  api: 30000
  global: 15000
  login: 10000
  healthCheck: 10000
  healthInterval: 60000

proxies: []
upstream: []
```
4. 构建并启动：
```bash
docker compose build
docker compose up -d
```

### 方式三：Go 源码直接运行（适合开发者）

环境要求：Go 1.26+ 且具备 C 编译链（Debian/Ubuntu 运行 `apt install build-essential`）。
```bash
mkdir -p config data
# 按方式二的说明在 config 文件夹下创建 config.yaml
go test ./...
go run ./cmd/emby-in-one
```

**默认访问地址**：
- Emby 客户端连接地址：`http://服务器IP:8096`
- 管理面板：`http://服务器IP:8096/admin`

---

## 进阶配置与核心原理

### 上游服务器认证

每台上游服务器只需提供两种认证方式之一：
1. **账号密码**：底层将调用 API 登录换取 Session Token。
2. **API Key**：直接免密请求（推荐）。

### 播放模式详解

此项可在全局设置或针对单台上游独立覆盖：
- **代理模式 (proxy)**：客户端流量全部经过您的 Emby-In-One 服务器转发。播放 `.m3u8` 会被重写为相对路径中转，自动映射虚拟 ID。适用于上游服务器禁止公网直连、或您希望对外隐藏上游真实地址的场景。
- **直连模式 (redirect)**：客户端点击播放时，代理下发 HTTP `302` 重定向，令客户端直接连接上游直链。此模式不消耗代理服务器带宽。

### 自定义 UA 伪装 (`spoofClient`)

为了防止被上游识别为代理客户端，支持以下策略：
- `passthrough`：将真实使用的客户端（例如用户的 iPhone Infuse）完整上报至上游，该标识具有记忆持久化能力。
- `infuse`：全局写死伪装为 Infuse 7.7.1 客户端特征。
- `custom`：自由定义上游见到的 `User-Agent`、`X-Emby-Client` 等 5 个核心指纹字段。
- `none`：不伪装，使用代理默认身份。

### 聚合去重与 ID 虚拟化

不同服务器上的影片之所以能合并，并且不妨碍用户针对特定服务器源的精准播放，是因为如下机制：
- **同源映射**：系统为合并后的影视项目统一派发一个全局唯一的虚拟 UUID。底层 SQLite `mappings.db` 保存着虚拟 ID 与所有上游其实际原始 ID 的从属关系。
- **媒体版本合并**：无论是电影还是剧集，当被判断为同一个物理影视作品（TMDB、标题一致）后，不同服务器的数据将被融合。您在播放器右下角选择“版本”时，正是在切换不同上游的数据流。

---

## 免责声明

> **注意**：本项目通过模拟 Emby 客户端行为与上游服务器通信，存在被上游或相关平台识别并封禁账号/API Key 的风险。使用本项目即表示您已自行承担上述风险，对于因使用不当或上游政策调整导致的封号及数据损失，作者不承担任何责任。

---

## 目录结构 (供开发者查阅)

```text
Emby-In-One/
├── AGENTS.md
├── README.md
├── README_EN.md
├── Update.md
├── Update Plan.md
├── SECURITY.md          # 安全指导
├── LICENSE
├── go.mod               # Go 后端依赖配置
├── cmd/                 # 程序入口
├── internal/            # 代理、去重、认证逻辑核心
├── third_party/sqlite/  # SQLite CGO 依赖
├── public/              # Vue 前端管理面板静态页面及依赖
├── src/                 # Node.js 历史遗留版本代码（仅作参考）
├── tests/               # Node.js 测试文件（仅作参考）
├── package.json         # Node.js 配置文件（无需启动）
├── Dockerfile           # Go 环境构建文件
├── docker-compose.yml 
├── install.sh           # Linux 自动化部署脚本
└── emby-in-one-cli.sh   # 终端管理面板脚本
```

## 许可证

GNU General Public License v3.0
