# UniFlow v1.6.1

Go + Gin + SQLite 搭的博客+社交系统。一个二进制文件就能跑，不用 MySQL、Redis 之类的外部依赖。

---

## 能干什么

**写文章** — Quill 富文本编辑器，有全屏模式、实时预览、草稿自动保存。支持定时发布、置顶、私密文章、分类标签和封面图。工具栏上除了基础格式还挂了提示框（4 种颜色）和彩色标签徽章，插图片/视频可以从媒体库里选已上传过的，不用重复传。

**发瞬间** — 类似朋友圈的动态，图文视频混合，支持草稿和定时发布。

**互动** — 文章评论、留言板、文章点赞。

**管理后台** — 文章/瞬间/评论的增删改查，媒体库批量管理，站点设置（Banner、关于我、设备展示、足迹地图等等），数据库备份恢复，操作日志，sitemap。

**RSS** — 文章和瞬间混排输出，阅读器可以直接订阅。

---

## 技术栈

Go 1.26 + Gin + SQLite（pure Go，不需要 CGO）+ Go template + Tailwind CSS v4（本地编译 + CDN 回退）+ Quill.js + Font Awesome 6

---

## 部署

```bash
git clone https://github.com/geekou/uniflow.git
cd uniflow
docker compose up -d
```

首次启动会编译 Go 源码，大概两分钟。编译完浏览器打开 `http://IP:9090/setup` 设置站点名和管理员账号就行。

端口：前台 `9090`，后台 `/admin`，RSS `/rss`。

> 单实例应用，会话存在内存里，别横向多副本跑。重启后重新登录就行。

### Docker Compose

```yaml
services:
  uniflow:
    build: .
    container_name: uniflow
    restart: unless-stopped
    ports:
      - "9090:9090"
    volumes:
      - ./data:/app/data
      - ./uploads:/app/uploads
      - ./backups:/app/backups
    environment:
      - GIN_MODE=release
      - PORT=9090
      - DB_PATH=/app/data/uniflow.db
```

更新：`git pull && docker compose up -d --build`

---

## 本地开发

```bash
go mod download
go run .
# 或者
go build -o uniflow.exe .

# 环境变量：GIN_MODE=debug / release，DB_PATH=自定义路径
```

---

## 项目结构

```
uniflow/
├── main.go
├── handlers/    # 前台 + 后台所有处理器
├── models/      # 数据模型 + 数据库初始化/迁移
├── utils/       # 备份、图片处理、sitemap
├── templates/   # Go template，partials/ 下面有 7 个复用片段
├── static/      # CSS、JS 依赖
└── uploads/     # 用户上传文件
```

---

## 更新记录

### v1.6.1 — 2026-07-06

- 工具栏精简：去掉了行内代码、清除格式（按钮图标显示异常），合并图片和视频入口，链接和图片按钮挪到前面
- 折叠面板和行动按钮砍了——Quill 对 `<details>` 和自定义 `<a>` 的处理有问题，编辑器里好好的，发布后格式全丢，折腾了好几轮决定不搞了
- 文章编辑器新增"保存草稿"按钮，编辑已有文章不再弹 localStorage 恢复提示，草稿编辑页默认走发布逻辑
- 瞬间支持草稿和定时发布，数据库加了 status / publish_at 字段，列表页改成表格布局，加了搜索和状态筛选
- 修了一堆 sed 批量删除导致的 JS 语法错误（工具栏全灭那种）
- 前台只显示已发布的瞬间

### v1.5 — 2026-07-05

- 提示框和标签徽章搞定，Quill Blot 注册 + insertEmbed，编辑器里和前台都正常渲染
- 文章编辑器加了全屏沉浸模式和实时预览，ESC 退出
- 媒体库选择器：发文章和瞬间时可以从已上传文件里选，不用重复传
- 文章详情页代码块加了复制按钮
- 留言板和关于我页面补齐了右侧栏，所有前台页面布局统一

### v1.4 — 2026-07-04

- 模板公共化：左栏、右栏、搜索栏、通知、移动端顶栏、回到顶部、底部脚本抽成 7 个 partials
- Tailwind CSS 本地静态编译，68KB，首屏不用等 CDN
- 系统暗色模式，跟系统设置自动切
- RSS 2.0 输出，文章和瞬间混排
- CSP 从 Report-Only 切到强制模式
- 数据库连接池 MaxOpenConns 1→3

### v1.3 — 2026-07-03

- 访问量去重：visitor_id Cookie + post_visits 表，同访客同篇文章每天只计一次
- bfcache 兼容：Cache-Control no-cache，返回不显示旧数据
- 热力图跨页面缓存，sessionStorage，切页面不闪
- 图片全站 lazy load，Banner eager 加载
- 回到顶部按钮 Iconify→Font Awesome，图标正常显示了
- 搜索页 Banner 不再随机变，修了空 TopPost 导致的降级逻辑

### v1.2 — 2026-07-02

- 备份页加本地上传备份文件功能，支持上传 tar.gz 恢复
- 评论支持登录用户上传图片
- 关于我页面重写：删掉测试信息卡片，设备展示和足迹地图保留，右侧栏补上
- 留言板补右侧栏，左侧栏宽度和其他页面统一
- 修复首页/文章页/瞬间页左侧栏宽度不一致、搜索框样式不统一等问题

### v1.1 — 2026-07-01

- Docker Compose 一键部署
- setup 引导页，首次部署设站点和管理员
- 后台仪表盘：文章/评论/用户统计，媒体库管理，用户角色管理
- 系统设置：站点名、Banner、BGM、关于我、设备展示、足迹地图
- 文章导出 Markdown、Sitemap 自动生成
- 安全响应头、文件上传校验、bluemonday HTML 过滤、操作日志

### v1.0 — 2026-06

- 第一个版本
- Go + Gin + SQLite，纯 Go 零 CGO
- 三栏布局、移动端适配
- 文章 + 瞬间 + 评论 + 留言板
- 安全审查修了 XSS/CSRF/SQL注入 等 10 个洞

---

MIT
