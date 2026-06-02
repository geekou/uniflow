# UniFlow

**Go + Gin + SQLite 构建的轻量级商业博客 + 社交混合系统**

无需 MySQL、Nginx、Redis 等外部依赖——一个二进制文件 + SQLite 即可运行，Docker 部署仅需 20MB 镜像。

---

## 功能特性

### 📝 内容管理
- Markdown 编辑器，支持草稿自动保存（本地缓存）
- 文章定时发布、置顶、隐私控制（公开/私密）
- 文章分类、标签、封面图、字数统计、阅读时长
- 文章导出为 Markdown 文件
- 自定义页面（关于、友链等）

### 💬 社交互动
- 文章评论（支持楼中楼回复、Emoji 表情）
- 留言板（支持图片 + 管理员回复）
- 瞬间/动态发布（图文 + 视频混合瀑布流）
- 文章点赞、赞赏（微信/支付宝二维码）

### 🎨 三栏式现代 UI
- Tailwind CSS v4 响应式布局
- 左侧菜单栏 + 中间内容区 + 右侧信息栏
- 大屏三栏独立滚动，中屏双栏粘性定位
- 暗色 Banner + 文章热度热力图
- 全文搜索、文章目录导航
- 移动端侧滑菜单 + 底部导航

### 🔒 安全特性
- bcrypt 密码哈希、HMAC-SHA256 Cookie 签名
- CSRF 同源验证、SameSite Strict Cookie
- 文件上传 MIME 白名单校验
- 评论/留言频率限制 + 敏感词过滤
- 登录爆破防护、安全响应头（XSS/Clickjacking 防护）
- 路径穿越防护、SQL 参数化查询

### ⚙️ 管理系统
- 后台仪表盘（文章/评论/用户统计）
- 文章/分类/标签/菜单 CRUD
- 媒体库管理（批量上传、删除）
- 用户管理（多角色：admin / editor）
- 系统设置（站点名称、Banner、BGM、敏感词等）
- 全站 tar.gz 备份/恢复
- Sitemap 自动生成、操作日志审计
- 首次部署引导页面（`/setup`）

---

## 技术栈

| 组件 | 技术 |
|------|------|
| 语言 | Go 1.26 |
| Web 框架 | Gin 1.12 |
| 数据库 | SQLite (modernc.org/sqlite，纯 Go，无需 CGO) |
| 模板引擎 | Go html/template |
| CSS | Tailwind CSS v4 CDN |
| 图标 | Font Awesome 6 + Iconify |
| 图片处理 | imaging（自动压缩 1600px / 80% 质量） |
| 密码加密 | bcrypt |
| HTML 过滤 | bluemonday |

---

## 快速开始

### 前置条件
- 服务器已安装 **Docker** 和 **Docker Compose**

### 一键部署

```bash
git clone https://github.com/geekou/uniflow.git
cd uniflow
docker compose up -d
```

> 首次启动会在容器内编译 Go 源码（约 2 分钟），编译完自动启动。

### 初始化

浏览器打开 `http://你的服务器IP:9090/setup`，设置：
- 站点名称
- 昵称
- 管理员用户名和密码（至少 6 位）

设置完成后自动跳转后台管理面板。

### 添加导航菜单

Setup 完成后左侧菜单是空的，进后台添加：

1. 打开 `http://IP:9090/admin/menus`
2. 依次添加四个菜单：

| 名称 | 路径 | 图标 | 排序 |
|------|------|------|------|
| 首页 | `/` | `fa-solid fa-house` | 1 |
| 文章 | `/posts` | `fa-solid fa-book` | 2 |
| 瞬间 | `/moments` | `fa-solid fa-bolt` | 3 |
| 留言板 | `/guestbook` | `fa-regular fa-envelope` | 4 |

图标名来自 [Font Awesome 6](https://fontawesome.com/icons)，直接复制即可。

### 默认端口
- 博客前台：`http://IP:9090`
- 后台管理：`http://IP:9090/admin`

---

## Docker Compose 配置

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

> 后续更新：`git pull && docker compose up -d --build`

反向代理建议配合 Nginx Proxy Manager 启用 HTTPS。

---

## 开发

```bash
# 安装依赖
go mod download

# 本地运行（默认端口 9090，Debug 需设 GIN_MODE=debug）
go run .

# 编译
go build -o uniflow.exe .

# 设置环境变量
# GIN_MODE=debug    → 开发模式（显示详细错误）
# GIN_MODE=release  → 生产模式
# DB_PATH=xxx       → 自定义数据库路径
```

---

## 项目结构

```
uniflow/
├── main.go              # 入口、路由注册、模板函数
├── handlers/
│   ├── handlers.go      # 前台处理器（首页、文章、搜索等）
│   ├── admin.go         # 后台管理处理器（CRUD、上传、备份等）
│   ├── middleware.go     # 中间件（认证、限流、CSRF、安全头）
│   └── stats.go         # 热力图统计 API
├── models/
│   ├── models.go        # 数据模型定义
│   └── db.go            # 数据库初始化、建表、迁移
├── utils/
│   ├── backup.go        # tar.gz 备份/恢复
│   ├── image.go         # 图片上传处理、压缩
│   └── sitemap.go       # Sitemap 生成
└── templates/           # Go html/template 模板（前台 + 后台）
```

---

## 更新日志

### v1.0 — 2026-06-02
- 首次发布
- 三栏式现代 UI，移动端适配
- 完整的博客 + 社交功能
- 安全审查通过（XSS/CSRF/SQL注入/文件上传等 10 项修复）
- Docker 一键部署支持

---

## License

MIT
