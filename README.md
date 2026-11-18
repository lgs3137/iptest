# iptest

> Cloudflare 优选 IP 测速 / 订阅生成 / DNS 推送 / 历史归档。内置 **Web 控制台**（默认 `:3000`），保留纯 CLI 模式，零第三方依赖。

[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](https://www.gnu.org/licenses/gpl-3.0)
[![Github Actions Badge](https://github.com/lgs3137/iptest/actions/workflows/Releases.yaml/badge.svg)](https://github.com/lgs3137/iptest/actions/workflows/Releases.yaml)

## Web页面示例

![image](img/iptest.png)

## 特性

- **测速**：TCP 延迟 → 真连接探测（国家 / 数据中心 / ASN / 出口 IP）→ 并发下载测速（MB/s）
- **多源采集**：本地路径 / `file://` / `http(s)://` 混合输入，明文与 Base64 订阅自动解析，递归发现新源并去重
- **排序策略**：先按延迟升序再测速；`speedtest=0` 时按 `rounds` 轮延迟均值排序
- **提前停止**：`min_speed` + `min_speed_count` 命中即停，省时省流量
- **订阅生成**：`ip` / `domain` 双类型，单模板通用，可选 Base64
- **DNS 推送**：Cloudflare A / AAAA 自动判断，存在即更新，支持记录查询与删除
- **历史归档**：每次追加前 N 名，按行数上限自动裁剪，可直接复用为输入源
- **Web 控制台**：全部参数可视化编辑、任务启停、阶段进度、实时日志（SSE）、结果 / 历史 / 订阅 / DNS / 文件 / 工具全操作
- **代理**：仅远程源下载与 DNS 推送走代理，测速恒直连
- **配置优先**：命令行 > `config.json` > 内置默认；配置缺失自动生成，每次保存自动备份 `.bak`

## 快速开始

```bash
go build -trimpath -ldflags "-s -w" -o iptest .

./iptest                     # Web 控制台 → http://127.0.0.1:3000/
./iptest -prefix /iptest     # 带路径前缀 → http://127.0.0.1:3000/iptest/
./iptest -cli -file "ip.txt,https://sub.example.com/list.txt"   # 纯命令行跑一次
```

首次运行自动生成 `config.json`。默认产物：结果 `html/result.csv`、归档 `html/bestip.csv`、订阅 `html/*.txt`。

推荐流程：**配置**页填输入源 → 保存 → **工具**页解析预览确认能提取到 IP → **控制台**点「完整流程」→ 看进度与实时日志 → **结果**页排序查看 → 按需开启订阅 / DNS。

## 项目结构

```
.
├── go.mod
├── iptest.go        # 配置 / 参数 / 测速引擎 / 多源采集 / 历史归档
├── subscription.go  # 订阅构建与生成（支持预览）
├── dns.go           # Cloudflare DNS 计划 / 推送 / 查询 / 删除
├── web.go           # 日志总线 + 任务状态机 + HTTP API + 内嵌前端
├── config.json      # 首次运行自动生成
└── html/            # 结果 / 归档 / 订阅输出目录
```

## 运行模式

| 场景 | 命令 |
|---|---|
| Web 控制台（默认） | `./iptest` |
| 命令行跑一次完整流程 | `./iptest -cli` |
| 只生成订阅 / 只推 DNS | `./iptest -sub` / `./iptest -dns` |
| 临时关闭 Web | `./iptest -web=false` |
| 换端口 / 只本机监听 / 加前缀 | `./iptest -port 8080 -host 127.0.0.1 -prefix /iptest` |
| 指定配置文件 | `./iptest -config /etc/iptest/config.json` 或 `CFIP_CONFIG=...` |

`-cli` / `-sub` / `-dns` 会强制走命令行；Web 与 CLI 共用同一套引擎与配置，Ctrl+C 均可优雅停止。

## 命令行参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-file` | "" | 输入源列表（逗号分隔），覆盖配置 `file` |
| `-outfile` | html/result.csv | 结果输出文件 |
| `-max` | 100 | TCP / probe / 延迟阶段并发 |
| `-speedtest` | 5 | 下载测速协程数，`0` 禁用测速 |
| `-url` | speed.cloudflare.com/__down?bytes=20000000 | 测速地址（可省略协议头） |
| `-delay` | 0 | 延迟阈值 ms，`0` 不过滤 |
| `-rounds` | 3 | 延迟轮数，仅 `speedtest=0` 生效 |
| `-minspeed` / `-minspeedcount` | 0 / 0 | 合格速度（MB/s）与合格数量，同时 >0 时提前停止 |
| `-historyenable` / `-history` | false / "" | 归档开关与文件（配置默认 `true`） |
| `-historycount` / `-historylimit` | 3 / 1000 | 每次归档条数 / 归档行数上限（`0` 不限） |
| `-depth` | 2 | 输入源递归抓取深度 |
| `-proxy` | "" | 代理地址，仅远程下载与 DNS 推送 |
| `-sub` / `-dns` / `-cli` | false | 仅订阅 / 仅 DNS / 强制命令行 |
| `-web` / `-host` / `-port` / `-prefix` | — | Web 开关与监听、路径前缀 |
| `-config` | "" | 配置文件路径 |

## 配置文件

配置查找顺序：`-config` → 环境变量 `CFIP_CONFIG` → 当前目录 → 可执行文件目录；均不存在时自动生成（`go run` 时落在当前目录）。

```jsonc
{
  "file": ["html/bestip.csv"],        // 本地路径 / file:// / http(s):// 可混用
  "outfile": "html/result.csv",
  "proxy": "",                        // socks5://127.0.0.1:10808 或 http://...
  "max": 100,
  "speedtest": 5,                     // 0 = 只测延迟
  "url": "speed.cloudflare.com/__down?bytes=20000000",
  "delay": 0,
  "min_speed": 0,                     // 单位 MB/s，支持小数
  "min_speed_count": 0,
  "rounds": 3,
  "history_enable": true,
  "history": "html/bestip.csv",
  "history_count": 3,
  "history_limit": 1000,              // 0 = 不限制（不推荐）
  "tcp_timeout_ms": 1000,             // TCP 拨号超时
  "probe_timeout_ms": 3000,           // 真连接探测超时
  "download_timeout_ms": 5000,        // 单 IP 下载超时
  "source_timeout_ms": 20000,         // 远程输入源下载超时
  "source_depth": 2,                  // 源中发现 URL 的递归深度
  "probe_url": "https://probe.609909.xyz/",  // SNI 自动取该地址域名
  "sub_config": {
    "enabled": false,
    "output_dir": "html/",            // 文件页仅允许浏览/下载此目录
    "count": 15,                      // IP 节点数量上限，0 = 全部
    "node_name_tpl": "{index}{Country}{CountryCode}{ip}",
    "b64encode": true,
    "subtype": ["ip", "domain"],
    "preferred_domains": ["1.example.com", "2.example.com"],
    "templates": [
      {
        "ip_file_name": "subip.txt",
        "domain_file_name": "subdomain.txt",
        "template": "vless://AreYouOK@{ip}{domain}:{port}?encryption=none&security=tls&sni=your.domain.com&fp=chrome&type=xhttp&host=your.domain.com&path=/AreYouOK&mode=auto#{node_name_tpl}"
      }
    ]
  },
  "dns_config": {
    "enabled": false,
    "token": "",                      // Cloudflare API Token
    "zone_id": "",
    "domain_tpl": "{index}.example.com",
    "count": 9,                       // 0 = 全部
    "ttl": 300,                       // 1 = 自动；0/负数按 300 处理
    "proxied": false                  // 优选 IP 建议灰云
  },
  "web_config": {
    "enabled": true,                  // 以下 5 项变更需重启生效
    "host": "0.0.0.0",
    "port": 3000,
    "path_prefix": "",                // 例 /iptest，页面与接口全部挂在该前缀下
    "auto_run": false,                // 启动即自动跑完整流程
    "log_buffer": 5000,               // 以下 3 项保存即生效
    "username": "admin",
    "password": ""                    // 留空 = 不鉴权
  }
}
```

所有相对路径以工作目录为基准，写出时父目录自动创建。

## Web 控制台

| 页签 | 能力 |
|---|---|
| 控制台 | 阶段进度、统计卡片、实时日志、四种任务启停 |
| 配置 | 全参数表单编辑、未保存变更提示、从磁盘重新载入、恢复默认 |
| 结果 | CSV 表格排序 / 搜索 / 分页 / 下载 |
| 历史归档 | 按批次分段展示、按上限清理、清空、下载 |
| 订阅 | 节点预览、生成文件、复制内容、订阅链接 |
| DNS | 推送计划预览、执行推送、现有记录查询与删除 |
| 文件 | 仅订阅输出目录的查看 / 下载 / Base64 解码 |
| 工具 | 输入源解析预览、代理测试（默认应用）、单 IP 探测 + 测速 |
| 日志 | 全量日志、级别与关键字过滤、debug 开关、导出 |

**鉴权与反代**：`password` 非空即启用 Basic Auth。设置 `path_prefix` 后，页面 `/iptest/`、接口 `/iptest/api/*`、订阅 `/iptest/sub/xxx.txt`，其余路径一律 404（仅 `/` 与 `/iptest` 会 302 到 `/iptest/`）。

```nginx
location /iptest/ {
    proxy_pass http://127.0.0.1:3000/iptest/;   # 前缀两端都保留，不要 rewrite
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Connection "";
    proxy_buffering off;        # SSE 实时日志必须关闭缓冲
    proxy_read_timeout 3600s;
}
```

## 订阅生成

- `subtype` 为空时：有 `preferred_domains` → `["ip","domain"]`，否则 `["ip"]`
- `count` 只限制 IP 节点；域名节点数量等于 `preferred_domains` 长度
- 每个模板可同时产出一个 IP 文件与一个域名文件，留空则不生成

| 模板变量 | 说明 |
|---|---|
| `{ip}` | 测速 IP（生成域名订阅时为空） |
| `{domain}` | 优选域名（生成 IP 订阅时为空） |
| `{port}` | 端口 |
| `{node_name_tpl}` | 节点名，由 `node_name_tpl` 渲染 |

`node_name_tpl` 支持 `{index}` `{ip}` `{port}` `{country}` `{countrycode}` `{domain}`（大小写不敏感）。域名节点默认以域名本身命名，除非模板中显式使用 `{domain}`。

输出：`b64encode=false` 每行一个节点；`true` 整段 Base64。

## DNS 推送

1. Cloudflare → My Profile → API Tokens → Create Token，模板 **Edit zone DNS**（`Zone.DNS:Edit`，指定域名）
2. 域名 Overview → API 处复制 **Zone ID**
3. 填入 `token` / `zone_id`，设置 `domain_tpl`

`domain_tpl` 支持 `{index}` `{ip}` `{country}` `{countrycode}` `{port}`；自动判断 A / AAAA，先按「域名 + 类型」查询，存在 `PUT` 更新、不存在 `POST` 新建。

## 历史归档

- 格式：每段一行 `# 时间` 注释 + 与 `result.csv` 相同的 9 列数据（无表头，延迟为整数毫秒）
- 裁剪：超过 `history_limit` 时从最早段删起，段内删空则连同注释行一并删除
- 复用：把归档路径加入 `file` 即可，`#` 注释行解析时自动忽略

## 代理边界

| 请求 | 走代理 |
|---|---|
| 远程输入源下载、DNS 推送、代理测试 | ✅ |
| 本地文件读取 | ❌ |
| probe 探测、下载测速（目标是待测 IP） | ❌ |

支持 `socks5://` `socks5h://` `http://` `https://`。启动时先验证代理，失败或非 200 自动回退直连。

## 输出结果

`result.csv` 固定 9 列：

```
IP地址,端口号,网络延迟ms,下载速度,国家,国家代码,数据中心,出站IP,ASN组织
104.16.1.1,443,42,12.34 MB/s,美国,US,LAX,104.16.1.1,"Cloudflare, Inc."
```

`speedtest=0` 时「下载速度」列为空，延迟为 `rounds` 轮均值并按延迟升序排序；否则按下载速度降序排序。

## API 一览

均位于 `path_prefix` 之下，返回 JSON。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/state` | 任务 / 进度 / 统计 / 代理 / 生效的 Web 参数 |
| GET · POST | `/api/config` | 读取（`?reload=1` 从磁盘重读）/ 保存 |
| POST | `/api/config/reset` | 恢复默认（旧配置备份 `.bak`） |
| POST | `/api/run` · `/api/stop` | 启动 `full·speedtest·sub·dns` / 取消 |
| GET · DELETE | `/api/logs` | 增量拉取 / 清空 |
| GET | `/api/events` | SSE：log / progress / state |
| GET | `/api/results` | 结果表（`file` `limit` `download=1`） |
| GET · POST | `/api/history` `/api/history/trim` `/clear` | 归档查看 / 清理 / 清空 |
| GET | `/api/files` · `/api/file` | 订阅目录列表 / 查看下载 |
| POST | `/api/sources/preview` `/api/proxy/test` `/api/ip/test` | 工具类 |
| POST | `/api/sub/preview` · `/api/sub/generate` | 订阅预览 / 生成 |
| POST · GET · DELETE | `/api/dns/plan` `/api/dns/records` | 计划 / 记录查询 / 删除 |
| GET | `/sub/{文件名}` | 订阅文件直链 |

## 构建

需要 Go 1.21+。

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o iptest .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o iptest.exe .
CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o iptest-linux-arm64 .
```

## FAQ 与注意事项

- **配置文件在哪？** 见上文查找顺序；移动二进制时请一并移动 `config.json`，或用 `-config` 指定。
- **`min_speed` 单位？** MB/s，`2` 即 2 MB/s，支持 `1.5`。
- **`speedtest=0` 为什么还要等？** 会对每个可用 IP 额外做 `rounds` 轮 TCP 延迟取均值，调小 `rounds` 可加速。
- **归档会无限增长吗？** 默认上限 1000 行自动裁剪；设 `0` 关闭限制不推荐。
- **DNS 报 403？** 检查 Token 的 `Zone.DNS:Edit` 权限、`zone_id` 与 `domain_tpl` 主域是否一致，建议用 API Token 而非 Global Key。
- **Linux 提示文件描述符偏低？** 启动前执行 `ulimit -n 10000`（或改 `/etc/security/limits.conf`）；Windows 无需处理。
- **反代后 404 / 日志不实时？** 前缀两端都要保留，且必须 `proxy_buffering off`。
- **安全**：`config.json` 含 CF Token，公网暴露务必设置 `web_config.password`，或只监听 `127.0.0.1` 再套反代。
- **任务互斥**：同一时刻只允许一个任务，冲突返回 409；停止任务时已完成部分仍会写入结果文件。
- **日志噪音**：逐 IP 探测失败记为 `debug`，前端默认隐藏，可在日志页开启。
- **路径含逗号**：`-file` 用逗号分隔，此类路径请写 `file:///data/a%2Cb.txt`。
- macOS 未签名二进制：`xattr -d com.apple.quarantine ./iptest`；UPX 压缩的 Windows 包可能被杀软误报。
- 请在法律允许范围内使用，禁止用于未授权的网络扫描或攻击。

## 致谢

- [Kwisma/iptest](https://github.com/Kwisma/iptest)
- [PoemMisty/CFData-WEB](https://github.com/PoemMisty/CFData-WEB)

## License

Copyright (C) 2026 PoemMisty — GNU GPL v3.0 or later，详见 `LICENSE`。