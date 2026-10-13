# chaoxing（原生 Go 分支）

本分支将命令行运行版本迁移为 Go，基于 [Samueli924/chaoxing](https://github.com/Samueli924/chaoxing) 及本 Fork 的 Python 修复版。保留 GPL-3.0 许可证、原作者署名和字体映射资源。

**Go 可执行文件不调用 Python，不需要 pip、Python、OCR、MySQL、Redis 或 Nginx。** Python 版本保留在本仓库 `main` 分支。

## 功能

- Cookie/账号密码登录、课程列表、课程文件夹、章节与任务卡片解析。
- Cookie 兼容旧请求头文本和 Python 修复版 JSON，保留域名、路径、过期及安全属性，原子保存为 0600 文件。
- 视频/音频、文档、阅读、直播、章节测验；普通签到与章节访问次数选项。
- 题库：AI（OpenAI 兼容）、SiliconFlow、TikuYanxi、TikuGo、TikuLike、TikuAdapter、TikuManual，多题库依次回退及 Go 答案缓存。
- 单选、多选、判断、填空、简答；加密 TTF 字体解析、测验保存/提交、覆盖率控制、官方成绩查询和可选的有限重做。
- 通知：ServerChan、Qmsg、Bark、Telegram。
- 默认四并发，有限重试、Ctrl+C/SIGTERM 取消、日志脱敏及文件轮转。

配置沿用 `config_template.ini`。首次建议保留 `use_cookies=true`，设置实际课程 ID。已有有效配置不用重新填写 API Key。

**403、验证码、人脸/位置等平台验证不自动破解。** 不切换网络、代理或参数绕过访问拒绝；账号密码遇到重定向明确停止并提示使用 Cookie。迁移语言不能保证修复原有 403 或账号密码登录问题。

## 与旧版的行为区别

- 测验仅保存、缺少答案、平台尚未确认通过时显示“待确认/仅保存”，不显示“全部完成”。
- 不随机猜测答案。没有答案的字段留空，按覆盖率决定保存或提交。
- 文档/阅读等请求返回 200 后重新检查卡片，仍有任务则保持待确认。
- 媒体进度按实际经过的时间记录，不在开始时发送整个视频时长。
- 字体无法解析、题型未知或官方成绩无法确认时明确失败，不继续猜测提交。
- 只支持正常签到，不处理位置、二维码、手势或验证码签到。章节访问不会发送伪装真人属性的监控心跳。
- 旧 `app.py` 只有 Flask/Celery 初始化，没有实际 Web/API 路由；本 Go 分支提供 CLI，不启动额外 Web 服务。
- Go 答案缓存为配置目录中的 `go-answer-cache.json`，不读取旧 Python 答案缓存。

## 直接编译运行

需要 Go 1.26 或更新版本；本次验证使用 Go 1.27.2。字体映射资源编译进可执行文件。

```bash
go mod download
go test ./...
go build -trimpath -ldflags="-s -w" -o chaoxing .
cp config_template.ini config.ini
# 在本地编辑 config.ini，写入实际课程 ID 等配置
# 将自己的 Cookie 请求头私下写入 cookies.txt，不要提交凭据
./chaoxing -c config.ini --check-login
./chaoxing -c config.ini --list-courses
./chaoxing -c config.ini
```

`--check-login` 只验证会话，`--list-courses` 只列出课程。退出码：0 成功；1 失败或存在待确认/跳过章节；2 配置错误；130 用户中断。

## Docker 部署

服务器上的旧容器先单独停止，避免两个版本同时处理同一账号。不要停止其他服务。

```bash
cd /opt/docker/projects/chaoxing
git status --short
git fetch origin
git switch --track origin/go
mkdir -p config logs
[ -f config/config.ini ] || cp config_template.ini config/config.ini
# 迁移已有有效配置，Cookie 放在 config/cookies.txt
chown -R 10001:10001 config logs
chmod 700 config logs
chmod 600 config/config.ini config/cookies.txt
docker compose -p chaoxing-go config --quiet
docker compose -p chaoxing-go build
docker compose -p chaoxing-go run --rm chaoxing --check-login -c /config/config.ini
docker compose -p chaoxing-go up -d --no-deps chaoxing
docker compose -p chaoxing-go logs --tail=100 -f chaoxing
```

如果本地已有 `go` 分支，用 `git switch go`、`git pull --ff-only origin go` 更新。若有未提交改动，先备份或保存改动再切换。

Docker 默认使用国内构建源：基础镜像通过 DaoCloud 公共镜像站，Go 模块使用 `https://goproxy.cn`，Alpine 包使用阿里云镜像。保留 HTTPS、`go.sum` 和 Go 校验数据库验证；不修改宿主 Docker 配置、不重启 Docker 服务。可通过 Compose 构建参数或 `CHAOXING_GO_BUILD_IMAGE`、`CHAOXING_GO_RUNTIME_IMAGE`、`CHAOXING_GO_MODULE_PROXY`、`CHAOXING_ALPINE_MIRROR` 环境变量替换源。

如果基础镜像已缓存，想继续使用原镜像并只更换依赖源：

```bash
CHAOXING_GO_BUILD_IMAGE=golang:1.27.2-alpine CHAOXING_GO_RUNTIME_IMAGE=alpine:3.23 docker compose -p chaoxing-go build
```

Go 依赖报错中出现 `proxy.golang.org` 超时，说明旧 Dockerfile 没配置国内 Go Proxy；更新本分支后重新构建，不需要 `--no-cache`。

来源：[DaoCloud 镜像站](https://github.com/DaoCloud/public-image-mirror)、[Goproxy.cn](https://github.com/goproxy/goproxy.cn)。本次环境访问国内站点被出口代理超时阻断，未验证用户服务器连通性，也未实测 Docker 镜像构建。

Docker 使用 Go 多阶段编译与缓存，最终镜像仅包含二进制、证书、时区和模板，不安装 Python 依赖。未设置 CPU/内存上限；保留非 root、只读根目录、无额外权限、独立网络、不开放端口。任务执行一次后退出，不自动循环重启。

**首次构建仍会下载 Go 构建镜像、运行镜像与 Go 模块，速度取决于网络。** 以后只改配置/Cookie 不需要构建，只改代码能使用模块与编译缓存。本机没有 Docker 守护进程，尚未验证真实 Docker 构建。

手动答题需要交互终端，执行 `docker compose -p chaoxing-go run --rm chaoxing`，不要和后台容器同时运行。

## 测试与验证边界

```bash
go test ./...
go test -race ./...
go vet ./...
go test -fuzz=FuzzCardsParser -fuzztime=5s .
go test -fuzz=FuzzFontParser -fuzztime=5s .
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o chaoxing .
```

本次验证：47 项单元测试通过；竞态检测与 `go vet` 通过；卡片解析模糊测试执行 42,471 次，字体解析模糊测试执行 95,113 次，均通过；Linux amd64、Linux arm64、Windows amd64 交叉编译通过。Linux amd64 静态二进制约 8.9MB。

测试使用本地模拟 HTTP 服务与合成 HTML/JSON/字形数据，覆盖 Cookie、登录重定向、403 不误报成功、不重试访问拒绝、有限重试、并发和取消、任务解析、题库和测验保存等。没有通过真实账号执行课程任务、测验提交、签到或通知；真实平台响应变化仍需脱敏日志继续维护。

## 来源与许可证

原项目：[Samueli924/chaoxing](https://github.com/Samueli924/chaoxing)。字体哈希方案来自 SocialSisterYi，并经 Samueli924 维护；Go 分支沿用原仓库 `resource/font_map_table.json`。GPL-3.0 许可证见 [LICENSE](LICENSE)。
