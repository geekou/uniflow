# UniFlow v1.6.5

Go + Gin + SQLite 搭的博客+社交系统。一个二进制文件就能跑，不用 MySQL、Redis 之类的外部依赖。

---

## 能干什么

**写文章** — Quill 富文本编辑器，有全屏模式、实时预览、草稿自动保存。支持定时发布、置顶、私密文章、分类标签和封面图。工具栏上除了基础格式还挂了提示框（4 种颜色）和彩色标签徽章，插图片/视频可以从媒体库里选已上传过的，不用重复传。

**发瞬间** — 类似朋友圈的动态，图文视频混合，支持草稿和定时发布。

**互动** — 文章评论、留言板、文章点赞。

**管理后台** — 文章/瞬间/评论的增删改查，媒体库批量管理，站点设置（Banner、关于我、设备展示、足迹地图等等），数据库备份恢复，操作日志，sitemap。足迹支持多照片上传和媒体库选择，悬浮城市弹窗横向滑动预览，点击照片在信息窗内放大。

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

### v1.6.5 — 2026-07
足迹支持多照片（上传+媒体库选择），悬浮城市弹窗横向滑动预览照片，点击照片在信息窗内放大查看。7 项安全修复：XSS 防 `<script>` 提前关闭、CSRF 三层校验、err.Error() 信息泄露过滤、Cookie Secure+SameSite 加固、菜单 icon 输入清洗。

### v1.6.1 — 2026-07
文章编辑器加了草稿保存，瞬间支持草稿和定时发布，工具栏去掉了显示异常的几个按钮，媒体库选择器复用。

### v1.5
提示框和标签徽章两个富文本组件落地，编辑器全屏预览，Quill Blot 注册机制跑通。

### v1.4
模板抽了 7 个公共片段，Tailwind CSS 本地编译，系统暗色模式，RSS 输出，CSP 强制拦截。

### v1.3
访问量去重，热力图缓存，全站图片懒加载，bfcache 兼容，搜索页 Banner 和回到顶部图标修复。

### v1.2
备份上传恢复，评论图片上传，关于我和留言板页面补齐，跨页面布局统一。

### v1.1
Docker 部署，setup 引导页，后台管理系统，安全加固。

### v1.0 — 2026-06
Go + Gin + SQLite，三栏布局，博客 + 社交核心功能，首次发布。

---

MIT
