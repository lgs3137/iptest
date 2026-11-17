# iptest

<p><a href="https://www.gnu.org/licenses/gpl-3.0"><img src="https://img.shields.io/badge/License-GPLv3-blue.svg" alt="License: GPL v3" /></a> <a href="https://github.com/lgs3137/iptest/actions"><img src="https://github.com/lgs3137/iptest/actions/workflows/Releases.yaml/badge.svg" alt="Github Actions Badge"></a></p>

一个用于 **Cloudflare 优选 IP 测速 / 订阅生成 / Cloudflare DNS 推送 / 历史归档** 的多功能命令行工具。

---

## 目录

- [功能特性](#功能特性)
- [项目结构](#项目结构)
- [快速开始](#快速开始)
- [输入源设计](#输入源设计)
- [命令行参数](#命令行参数)
- [配置文件详解](#配置文件详解)
- [工作模式](#工作模式)
- [订阅生成](#订阅生成)
- [DNS 推送](#dns-推送)
- [历史最优归档](#历史最优归档)
- [代理支持](#代理支持)
- [速度合格提前停止](#速度合格提前停止)
- [多源递归提取](#多源递归提取)
- [路径支持](#路径支持)
- [输出结果](#输出结果)
- [构建与发布](#构建与发布)
- [常见问题](#常见问题)
- [注意事项](#注意事项)

---

## 功能特性

- 🚀 **IP 延迟测速**：TCP 三次握手延迟测量，可通过 `-delay` 阈值过滤
- 🌍 **信息探测**：候选 IP 真连接验证（TLS SNI 探测）并获取 **数据中心 / 国家 / ASN / 出口 IP**
- 📶 **下载测速**：并发下载测速，单位 **MB/s**，可按合格速度提前停止
- 🔗 **多源递归提取**：支持明文订阅、Base64 订阅、任意文本，支持 `http(s)://`、`file://` 及本地路径混合输入，并在内存中递归发现新来源
- 📝 **订阅生成**：同时支持 `ip`（测速 IP）和 `domain`（优选域名）两种节点类型，可选 Base64 编码
- ☁️ **Cloudflare DNS 推送**：自动判断 A / AAAA 记录，存在则更新，不存在则新建
- 🗂 **历史最优归档**：每次测速后自动把排名前 N 的结果追加到历史文件，并可设置最大行数避免无限增长，历史文件可直接作为输入源复用
- 🔌 **代理支持**：远程输入源下载与 DNS 推送可走 `socks5://` 或 `http://` 代理，测速仍保持直连
- ⚙️ **配置优先**：命令行参数 > `config.json` > 内置默认值，配置文件缺失时自动生成
- 🖥️ **多平台**：Linux / Windows / macOS 全平台、多架构发布（含 UPX 压缩）

---

## 项目结构

```
.
├── iptest.go         # 主入口、测速、多源采集、代理初始化、历史归档
├── subscription.go   # 订阅文件生成
├── dns.go            # Cloudflare DNS 推送
├── config.json       # 配置文件（首次运行自动生成）
└── html/             # 资源目录
```

---

## 快速开始

### 1. 准备输入

`config.json` 中的 `sources` 数组支持本地与远程源的任意组合：

```json
{
  "sources": [
    "html/bestip.csv",
    "https://sub.example.com/list.txt",
    "/opt/iptest/data/ips.txt",
    "file:///opt/iptest/history/bestip.csv"
  ]
}
```

每个元素可以是：
- **本地文件**：`ip.txt`、`/opt/x.txt`、`./data/y.txt`、`C:\z.txt`
- **file:// URL**：`file:///opt/x.txt`
- **远程 URL**：`https://...`、`http://...`

程序自动识别每个源的类型，本地读盘、远程下载，直接在内存中统一提取并去重 IP（无需生成中间文件）。

### 2. 运行

```bash
# 全流程（读取 config.json 或自动生成）
./iptest

# 命令行显式指定输入源（覆盖配置）
./iptest -sources "ip.txt,https://sub.example.com/list.txt"

# 命令行单个本地文件（兼容旧版 -file，追加到 sources）
./iptest -file ip.txt

# 命令行输入源列表（兼容旧版 -urls，追加到 sources）
./iptest -urls "https://a.com/x.txt,file:///opt/y.txt"

# 只读测速结果生成订阅
./iptest -sub

# 只读测速结果推送 DNS
./iptest -dns

# 开启历史归档
./iptest -historyenable -history bestip.csv -historycount 5
```

首次运行会在可执行文件所在目录生成 `config.json`，并继续执行主逻辑。

### 3. 查看结果

- 测速结果：`result.csv`
- 历史归档：`bestip.csv`（开启后）
- 订阅文件：`html/` 目录下的 `.txt` 文件（默认 base64 编码）

---

## 输入源设计

**`sources` 数组**是输入源的核心入口。程序对每个元素执行以下流程：

1. **加载内容**：本地读盘 / 远程下载（若启用代理则走代理）
2. **尝试 Base64 解码**：如果是 Base64 编码内容，自动解码后与原文一并参与提取
3. **提取 IP**：
   - 行格式为 `IP 端口`（空格分隔）时：保留原端口（如 `1.1.1.1 8443`）
   - 否则走等价 awk 逻辑：提取有效 IPv4 并默认匹配 `443` 端口（忽略非法段及非 443 端口）
4. **递归发现新来源**：文本中出现 `http(s)://` 或 `file://` 时自动入队继续处理（最大递归深度 ≤ 2）

**优先级规则**：

| 命令行参数 | 行为 |
|------------|------|
| 未传任何源参数 | 使用配置文件 `sources`（如为空则默认 `["ip.txt"]`） |
| 传 `-sources` | **完全覆盖**配置文件的 `sources` |
| 传 `-file` / `-urls` | **追加**到当前 `sources` 列表（兼容旧版参数） |

**去重**：所有源处理完成后，最终提取的 `IP:Port` 列表全局去重。

**向后兼容**：配置文件中的 `file` 和 `urls` 字段仍可读取，会自动合并到 `sources`。建议迁移到新格式：

```jsonc
// 旧格式（仍兼容）
{
  "file": "ip.txt",
  "urls": "https://x.com/a.txt"
}

// 新格式（推荐）
{
  "sources": ["ip.txt", "https://x.com/a.txt"]
}
```

---

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-sources` | `""` | 输入源列表（逗号分隔），本地或 URL，自动识别；**覆盖**配置文件 `sources` |
| `-file` | `""` | 输入文件（单个，兼容旧版；追加到 `sources`） |
| `-urls` | `""` | 输入源列表（逗号分隔，兼容旧版；追加到 `sources`） |
| `-outfile` | `ip.csv` | 测速结果输出文件，支持路径 |
| `-max` | `100` | TCP / probe 阶段并发协程数 |
| `-speedtest` | `5` | 下载测速并发协程数，`0` 表示禁用测速 |
| `-url` | `speed.cloudflare.com/__down?bytes=20000000` | 测速文件地址（不带 `https://` 协议头） |
| `-delay` | `0` | TCP 延迟阈值（ms），`0` 禁用过滤 |
| `-sub` | `false` | **只**读取测速结果并生成订阅 |
| `-dns` | `false` | **只**读取测速结果并推送 DNS |
| `-proxy` | `""` | 代理地址，如 `socks5://127.0.0.1:10808`，仅远程下载与 DNS 推送走代理 |
| `-minspeed` | `0` | 下载速度合格线（**MB/s**，支持小数如 `1.5`），`0` 不限制 |
| `-minspeedcount` | `0` | 合格 IP 数量上限，达到后提前停止测速，`0` 不限制 |
| `-historyenable` | `false` | 是否启用历史归档 |
| `-history` | `""` | 历史归档文件路径，如 `bestip.csv` |
| `-historycount` | `3` | 每次归档前 N 个测速结果 |
| `-historylimit` | `1000` | 历史归档文件最大数据行数，超出后清理较早数据，`0` 不限制 |

> ⚠️ `-minspeed` 单位是 **MB/s**。`2` 表示 2 MB/s。

---

## 配置文件详解

```jsonc
{
  "sources": [
    "html/bestip.csv"
  ],
  "outfile": "html/result.csv",
  "proxy": "",                  // socks5://127.0.0.1:10808 或 http://...
  "max": 100,
  "speedtest": 5,
  "url": "speed.cloudflare.com/__down?bytes=20000000",
  "delay": 0,
  "min_speed": 0,               // 单位 MB/s
  "min_speed_count": 0,

  "history_enable": false,      // 是否启用历史归档
  "history": "html/bestip.csv", // 归档文件路径，例如 "bestip.csv" 或 "/opt/iptest/history/bestip.csv"
  "history_count": 3,           // 每次归档前 N 个，默认 3
  "history_limit": 1000,        // 历史文件最大数据行数，0 表示不限制

  "sub_config": {
    "enabled": false,           // 测速完成后是否自动生成订阅
    "output_dir": "./html",
    "count": 15,                // ip 类型节点的数量上限
    "node_name_tpl": "{index}{Country}{CountryCode}{ip}",
    "b64encode": true,
    "templates": [ /* 见下文 */ ]
  },

  "dns_config": {
    "enabled": false,           // 测速完成后是否自动推送 DNS
    "token": "cfut_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    "zone_id": "yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy",
    "domain_tpl": "{index}.example.com",
    "count": 9,
    "ttl": 300,                 // 5分钟
    "proxied": false            // 是否开启小黄云
  }
}
```

**优先级**：命令行显式传入 > `config.json` > 内置默认值。

**配置文件位置**：与可执行文件同目录（`filepath.Dir(os.Executable())`）。若不存在，首次运行自动生成默认配置。

**路径字段**：`sources`（内嵌本地路径）/ `outfile` / `history` / `sub_config.output_dir` 均支持相对或绝对路径；写出时父目录不存在会自动创建。

---

## 工作模式

| 命令 | 行为 |
|------|------|
| `./iptest` | 加载配置 → 采集并去重 `sources` 中的 IP → 测速 → 写 `outfile` → 若 `history_enable` 则归档 → 若 `sub_config.enabled` 则生成订阅 → 若 `dns_config.enabled` 则推送 DNS |
| `./iptest -sub` | 加载配置 → 读 `outfile` → 生成订阅（要求 `sub_config.enabled=true`） |
| `./iptest -dns` | 加载配置 → 读 `outfile` → 推送 DNS（要求 `dns_config.enabled=true`，支持走代理） |
| `./iptest -sources "..."` | 覆盖配置中的 `sources`，提取 IP → 进入测速主流程 |
| `./iptest -historyenable -history bestip.csv` | 主流程结束后追加历史归档 |

---

## 订阅生成

### 模板变量

`template` 字段支持以下占位符（**大小写不敏感**）：

| 变量 | 说明 |
|------|------|
| `{ip}` | 测速 IP |
| `{domain}` | 优选域名（仅 `subtype=domain` 时有效） |
| `{port}` | 端口 |
| `{node_name_tpl}` | 节点名称（由 `node_name_tpl` 渲染后填入；`domain` 类型时默认使用域名本身） |

`node_name_tpl` 字段支持以下占位符（**大小写不敏感**）：

| 变量 | 说明 |
|------|------|
| `{index}` | 序号（从 1 开始） |
| `{ip}` | 测速 IP |
| `{port}` | 端口 |
| `{country}` / `{Country}` | 国家中文名，如 `美国` |
| `{countryCode}` / `{CountryCode}` | 国家代码，如 `US` |

### 模板示例

**IP 节点**（数量由 `count` 控制）：

```json
{
  "file_name": "subip.txt",
  "template": "vless://AreYouOK@{ip}:{port}?encryption=none&security=tls&sni=your.domain.com&fp=chrome&type=xhttp&host=your.domain.com&path=%2FAreYouOK&mode=auto#{node_name_tpl}",
  "subtype": "ip",
  "preferred_domains": []
}
```

**域名节点**（数量由 `preferred_domains` 长度控制）：

```json
{
  "file_name": "subdomain.txt",
  "template": "vless://AreYouOK@{domain}:{port}?encryption=none&security=tls&sni=your.domain.com&fp=chrome&type=xhttp&host=your.domain.com&path=%2FAreYouOK&mode=auto#{domain}",
  "subtype": "domain",
  "preferred_domains": [
    "1.example.com",
    "2.example.com",
    "3.example.com"
  ]
}
```

### 输出

- 明文（`b64encode=false`）：每行一个节点
- Base64 编码（`b64encode=true`）：整段 Base64 编码输出

---

## DNS 推送

读取测速结果，取排名前 `count` 个 IP 推送到 Cloudflare：

- **域名模板**：`domain_tpl` 中的 `{index}` 替换为序号（从 1 开始）
- **记录类型**：自动判断 `A`（IPv4）或 `AAAA`（IPv6）
- **更新策略**：先按域名+类型查询，存在则 `PUT` 更新，不存在则 `POST` 新建
- **TTL**：`ttl=0` 时使用默认值 `300`；Cloudflare 特殊值 `1` 表示自动
- **Proxied**：优选 IP 场景建议设为 `false`（灰云）

### 创建 Cloudflare API Token

1. 登录 [Cloudflare Dashboard](https://dash.cloudflare.com/)
2. 进入 **My Profile → API Tokens → Create Token**
3. 模板选择 **Edit zone DNS**
4. 权限：`Zone.DNS` = **Edit**，Zone Resources 选定具体域名
5. 将生成的 Token 填入 `token`，目标域名的 Zone ID 填入 `zone_id`

Zone ID 在域名管理页的 **Overview → API** 右侧可见。

---

## 历史最优归档

每次测速完成后，可把排名前 N 的结果**追加**到指定文件，便于跨次对比并作为下次输入源复用。

### 启用方式

配置文件：

```jsonc
{
  "history_enable": true,
  "history": "history/bestip.csv",
  "history_count": 5,
  "history_limit": 1000
}
```

命令行：

```bash
./iptest -historyenable -history "history/bestip.csv" -historycount 5 -historylimit 1000
```

### 参数说明

| 参数 | 说明 |
|------|------|
| `history_enable` / `-historyenable` | 是否启用归档 |
| `history` / `-history` | 归档文件路径 |
| `history_count` / `-historycount` | 每次归档前 N 个 |
| `history_limit` / `-historylimit` | 归档文件最大数据行数，`0` 表示不限制 |

### 归档格式

- **无表头**
- 每组前一行 `# 时间`（注释行，再次作为输入源时会被自动忽略）
- 数据行与 `ip.csv` 相同 9 列（延迟为纯整数毫秒）

示例：

```
# 2026-11-12 17:17:22
8.212.8.26,443,28,27.25 MB/s,中国香港,HK,HKG,8.217.220.231,Aliyun Computing Co.LTD
134.185.91.72,443,285,12.89 MB/s,新加坡,SG,SIN,2603:c024:4507:ccbb:b:b:b:b,Oracle Corporation
43.159.4.43,443,35,12.08 MB/s,中国香港,HK,HKG,43.135.96.24,6 COLLYER QUAY
```

### 超限清理机制

开启 `history_limit`（默认 1000）后，每次追加完成后会自动检查文件总数据行数：

- 数据行总数 ≤ `history_limit`：不处理
- 数据行总数 > `history_limit`：从**最早的段**开始删除数据行，直到总数等于 `history_limit`
- 若某段的数据行被删光，其 `# 时间` 注释行也一并删除（避免孤立注释）
- 文件内每个仍保留的段都至少有一行数据

**示例**（`history_limit = 3`）：

追加前的文件：

```
# 2026-11-10 10:00:00
1.1.1.1,443,10,5.00 MB/s,...
2.2.2.2,443,20,4.00 MB/s,...
# 2026-11-11 11:00:00
3.3.3.3,443,15,6.00 MB/s,...
```

本次追加 `history_count=2` 后，数据行共 5 条，超出 3 行上限：删除最早段的前 2 行，结果为：

```
# 2026-11-11 11:00:00
3.3.3.3,443,15,6.00 MB/s,...
# 2026-11-12 12:00:00
4.4.4.4,443,25,7.00 MB/s,...
5.5.5.5,443,30,3.00 MB/s,...
```

日志输出：

```
[history] 已归档前 2 个结果到 bestip.csv
[history] 历史文件已超出 3 行上限, 清理较早数据 2 行
```

### 行为规则

| 场景 | 行为 |
|------|------|
| `history_enable=false` | 不归档，忽略 `history` / `history_count` / `history_limit` |
| `history_enable=true` 且 `history=""` | 不归档，无日志输出 |
| `history_enable=true` 且 `history` 父目录不存在 | 自动创建 |
| `history_limit = 0` | 不裁剪，无限累积（谨慎使用） |
| `history_limit > 0` | 每次追加后裁剪到不超过 N 行 |
| `history_count > history_limit` | 裁剪后仅保留最新 `history_limit` 行 |
| 结果为空 / `speedtest=0` | 不归档（无下载速度数据） |
| 写入失败 | 打印错误日志，原文件保持不变（临时文件被清理） |
| Windows 换行 `\r\n` | 兼容处理 |

### 归档文件复用为输入源

把归档文件加入 `sources`，下次测速时优先复用历史最优：

```jsonc
{
  "sources": [
    "file:///opt/iptest/history/bestip.csv",
    "https://sub.example.com/list.txt"
  ]
}
```

---

## 代理支持

在配置中提供 `proxy`，或通过 `-proxy` 指定：

非空启动时会先通过该代理进行连通性验证：

- 返回 **200** → 启用代理
- 请求失败 / 非 200 → 打印告警，**自动回退直连**

**代理作用范围**：

| 请求类型 | 是否走代理 |
|---------|-----------|
| `sources` 远程 URL 下载 | ✅ |
| DNS 推送（查询 + 提交） | ✅ |
| `sources` 读取本地文件 | ❌ 直接读盘 |
| probe 探测（连接待测 IP） | ❌ 直连 |
| 下载测速（连接待测 IP） | ❌ 直连 |

> 之所以测速相关请求不走代理，是因为这些请求的拨号目标是**待测 IP**，走代理会失去测速意义。

支持协议：`socks5://`、`socks5h://`、`http://`、`https://`（由 Go `net/http` 原生支持）。

---

## 速度合格提前停止

同时设置 `-minspeed` 和 `-minspeedcount`（或配置中的 `min_speed` / `min_speed_count`）即可启用：

- 每完成一次下载测速，若 `>= min_speed`，合格计数 +1
- 合格计数达到 `min_speed_count` 时，剩余待测 IP 不再测速
- 最终输出仍按 **下载速度降序** 排序（若禁用下载测速 `-speedtest 0` 则按延迟升序排序）
- 若全量测完仍不足 `min_speed_count`，以实际数量为准

**示例**：

```bash
# 找到 9 个速度 >= 2 MB/s 的 IP 就停止
./iptest -minspeed 2 -minspeedcount 9
```

```jsonc
{
  "min_speed": 2,
  "min_speed_count": 9
}
```

---

## 多源递归提取

程序启动时按队列顺序处理 `sources` 中的所有输入源（全内存处理，不落盘中间文件）：

1. 逐个加载来源内容（明文 / Base64 均可），来源可以是：
   - `http://` / `https://` 远程 URL
   - `file:///abs/path` 本地文件
   - 裸本地路径，如 `/opt/x.txt`、`./data/y.txt`、`C:\z.txt`
2. 提取有效 IP 列表：
   - 若行格式为 `IP 端口`，保留其指定端口
   - 否则用等价 awk 逻辑提取 IPv4（忽略段值 > 255 的非法 IP、忽略非 443 端口）并附加 `443` 端口
3. 对文本中出现的 `http(s)://` 与 `file://` 继续入队递归（最大深度 ≤ 2）
4. 全局去重后直接送入并发测速流程

**输出示例**：

```
[source] 待处理输入源: 2 个
[source]   1. [本地] file:///opt/iptest/history/bestip.csv
[source]   2. [远程] https://sub.example.com/list.txt
[source] (1) 本地 深度=0: file:///opt/iptest/history/bestip.csv
[source]     └─ 新增 IP: 15, 累计唯一 IP: 15
[source] (2) 远程 深度=0: https://sub.example.com/list.txt
[source]     └─ 新增 IP: 5, 累计唯一 IP: 20
[source]     └─ 发现新来源: 1, 队列剩余: 1
[source] (3) 远程 深度=1: https://example.com/extra.txt
[source]     └─ 新增 IP: 3, 累计唯一 IP: 23
[source] 完成: 共处理 3 个来源, 提取唯一 IP: 23 个
[source] 共提取 23 个有效 IP 用于测速
```

---

## 路径支持

`sources`（本地路径）、`outfile`、`history`、`sub_config.output_dir` 均支持相对与绝对路径：

```bash
# 输入路径带目录
./iptest -sources ./data/ips.txt

# 输出路径带目录（父目录不存在会自动创建）
./iptest -outfile /opt/iptest/out/ip.csv

# 历史文件路径带目录（父目录不存在会自动创建）
./iptest -historyenable -history ./history/bestip.csv
```

配置文件同样支持：

```jsonc
{
  "sources": ["/opt/iptest/data/ip.txt"],
  "outfile": "/opt/iptest/out/ip.csv",
  "history": "/opt/iptest/history/bestip.csv"
}
```

**目录自动创建规则**：

| 字段 | 何时自动创建父目录 |
|------|-------------------|
| `outfile` | 测速结果写出时 |
| `history` | 归档追加时 |
| `sub_config.output_dir` | 生成订阅文件时 |

> 若 `sources` 中的本地文件不存在，会打印 `[source] 加载失败` 日志并跳过该源；若所有源均未提取到有效 IP，程序将提示并退出。

---

## 输出结果

### 测速结果 `ip.csv`

固定 9 列，与是否测速无关（禁用下载测速 `-speedtest 0` 时"下载速度"列为空）：

```csv
IP地址,端口号,网络延迟ms,下载速度,国家,国家代码,数据中心,出站IP,ASN组织
104.16.1.1,443,42,12.34 MB/s,美国,US,LAX,104.16.1.1,"Cloudflare, Inc."
104.16.2.2,443,48,8.91 MB/s,美国,US,SJC,104.16.2.2,"Cloudflare, Inc."
```

| 列 | 来源 | 说明 |
|----|------|------|
| IP地址 | 输入 | 待测 IP |
| 端口号 | 输入 | 待测端口 |
| 网络延迟ms | TCP 握手 | 纯整数毫秒（不带 `ms` 后缀） |
| 下载速度 | 下载测速 | 单位 **MB/s**，保留两位小数 |
| 国家 | probe.Country | 中文名（如"美国"） |
| 国家代码 | probe.Country | ISO 3166-1 alpha-2（如"US"） |
| 数据中心 | probe.Colo | Cloudflare 三字码（如"LAX"） |
| 出站IP | probe.IP | Cloudflare 看到的出口 IP |
| ASN组织 | probe.ASOrganization | AS 组织名称 |

### 历史归档文件 `bestip.csv`

- 无表头，每段前有 `# 时间` 注释行
- 数据列与 `ip.csv` 完全一致（9 列，延迟为纯整数毫秒）
- 追加写入，跨多次运行累积
- 可通过 `history_limit` 限制最大行数，自动裁剪较早数据

### 订阅文件

- `sub_config.output_dir` 目录下，文件名由 `file_name` 指定
- 默认 **Base64 编码**（`b64encode: true`）
- 每个模板生成一个独立文件

---

## 构建与发布

### 本地构建

需要 Go **1.21+**。

```bash
# 编译
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o iptest .

# 交叉编译示例
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w" -o iptest-windows-amd64.exe .
```

### 发布产物一览

| 文件名示例 | 平台 | 架构 |
|-----------|------|------|
| `iptest-v1.0.0-linux-amd64.tar.gz` | Linux | x86_64 |
| `iptest-v1.0.0-linux-arm64.tar.gz` | Linux | ARM64 |
| `iptest-v1.0.0-linux-armv7.tar.gz` | Linux | ARMv7 |
| `iptest-v1.0.0-linux-386.tar.gz` | Linux | x86 |
| `iptest-v1.0.0-windows-amd64.zip` | Windows | x86_64 |
| `iptest-v1.0.0-windows-arm64.zip` | Windows | ARM64 |
| `iptest-v1.0.0-windows-386.zip` | Windows | x86 |
| `iptest-v1.0.0-darwin-amd64.tar.gz` | macOS | Intel |
| `iptest-v1.0.0-darwin-arm64.tar.gz` | macOS | Apple Silicon |

---

## 常见问题

### Q: 首次运行提示"配置文件不存在"？

正常现象，会自动生成默认 `config.json` 并继续执行。请按需修改其中的 `sub_config` / `dns_config` 后再启用对应功能。

### Q: `min_speed` 单位是 kB/s 还是 MB/s？

**MB/s**。例如想找到速度 ≥ 2 MB/s 的 IP，请设 `min_speed: 2` 或 `-minspeed 2`。

### Q: `-sources` 支持本地文件吗？

支持。多种写法可混合使用：

```bash
./iptest -sources "file:///opt/iptest/all.txt"
./iptest -sources "/opt/iptest/all.txt"
./iptest -sources "https://sub.example.com/a.txt,file:///opt/iptest/local.txt"
```

本地文件不走代理，直接读取磁盘；即使配置了 `proxy` 也不影响。

### Q: 历史归档文件能直接作为输入源复用吗？

可以。历史文件每段前的 `# 时间` 是注释行，提取解析时会自动忽略。直接把路径加入 `sources` 即可：

```jsonc
{
  "sources": ["file:///opt/iptest/history/bestip.csv"]
}
```

### Q: 归档文件会无限增长吗？

不会。默认 `history_limit=1000`，每次追加完成后自动裁剪到最多 1000 条数据行，删除最早的记录。若想保留更多可调大该值，或设为 `0` 关闭限制（不推荐）。

### Q: 裁剪会破坏 `# 时间` 段的完整性吗？

不会。裁剪以"段"为单位（`# 时间` 行 + 其下数据行）。当某段的数据行全被删光时，其 `# 时间` 注释行也会一并删除，不会留下孤立注释。

### Q: 配置文件放在哪？

与可执行文件同目录。若你移动了二进制，请同时移动 `config.json`，或每次用命令行参数显式指定。

### Q: DNS 推送报 403？

- 确认 `token` 具备 `Zone.DNS:Edit` 权限
- 确认 `zone_id` 与 `domain_tpl` 的主域名一致
- 建议使用 API Token 而非 Global Key

### Q: Windows 下如何以管理员权限运行？

打开 PowerShell **以管理员身份运行**，`cd` 到工具所在目录，执行：

```powershell
.\iptest.exe
```

### Q: 为什么 Linux 启动时打印"提升文件描述符上限时出现错误"？

测速需要大量并发连接，程序会自动尝试执行 `ulimit -n 10000`。若非 root 权限运行可能会失败，但不影响主程序运行（仅在极高并发时可能受限）。可在 `/etc/security/limits.conf` 手动调高。

---

## 注意事项

1. **代理仅覆盖 `sources` 远程下载和 DNS 推送**：测速过程一定直连（否则测速结果无意义）；`sources` 读取本地文件也不走代理。
2. **UPX 压缩的 Windows 二进制可能被部分杀毒软件误报**：这是 UPX 的常见现象，如有顾虑可自行去掉 UPX 压缩。
3. **macOS 二进制未签名**：首次运行可能提示"无法验证开发者"，执行：
   ```bash
   xattr -d com.apple.quarantine ./iptest
   ```
   或 **系统设置 → 隐私与安全性 → 仍要打开**。
4. **Cloudflare 免费账户 TTL 最小值 60 秒**：`ttl=1` 表示自动（300s）。
5. **`sources` 逗号分隔**：如果本地路径本身含逗号（如 `/data/a,b.txt`），建议用 `file:///data/a%2Cb.txt` 形式（URL 解析会自动还原 `%2C` → `,`）。
6. **`history_limit` 建议不要设为 0**：长期运行会导致归档文件越来越大，影响解析性能。
7. **请在法律允许的范围内使用本工具**，禁止用于任何未授权的网络攻击或扫描。

---

## 致谢

- GitHub：[Kwisma/iptest](https://github.com/Kwisma/iptest)
- GitHub：[PoemMisty/CFData-WEB](https://github.com/PoemMisty/CFData-WEB)

## License

Copyright (C) 2026 PoemMisty

This project is licensed under the GNU General Public License v3.0 or later.
See the LICENSE file for details.
