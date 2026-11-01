package today

// 记忆（Memory）的读写。
//
// 记忆内容的真正宿主是 WebDAV：桌面端发 PROPFIND /memory（Depth: infinity），
// 用 DOMParser 解 207。但那条 DAV 路由挂在 api.todayai.cn 上，本机出口被 AWS ALB
// 403，唯一能通的 /api/cloud 又是 Next 的 rewrite —— 实测承载不了 DAV：
//
//	无 body          → 边缘 400 "Bad Request"（11B）
//	Content-Length:0 → Next 自己的 400 HTML
//	带 body          → 转发后 72s 超时 504
//
// 所以 DAV 写（PUT/MKCOL/MOVE/DELETE）在本机网络下结构性不可达。
// 但还有一条 REST 通道能通，而且是官方的：
//
//	POST /v1/files             multipart 上传 → 201 {file:{fileRef:{fileId,versionId}}}
//	POST /v1/memories/upload   {"fileRef":{…}} → 202 {task:{id,status:"queued"}}
//	GET  /v2/tasks?limit=N     轮询任务，completed 后 agent 已把内容写进 Memory
//
// 注意语义：这是「导入」而不是「覆写」。上游会让 agent 抽取内容、自己决定
// 落到哪个目录、并重写格式（实测会把上传的 note.md 存成 notes/test-memory.md，
// 加 frontmatter，把来源和日期记进去）。所以它适合「记一条」，不适合「改一行」。
//
// 同一个 fileRef 重复提交是幂等的：返回同一个 task id，status 200 + idempotent:true。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

// FileRef 是对象存储里的确切版本引用，上传与导入都以它为准。
type FileRef struct {
	FileID    string `json:"fileId"`
	VersionID string `json:"versionId"`
}

// UploadedFile 是 POST /v1/files 的返回。
type UploadedFile struct {
	ID          string  `json:"id"`
	Path        string  `json:"path"`
	StoragePath string  `json:"storagePath"`
	URL         string  `json:"url"`
	Filename    string  `json:"filename"`
	MimeType    string  `json:"mimeType"`
	Size        int64   `json:"size"`
	FileRef     FileRef `json:"fileRef"`
}

// MemoryPin 是被钉住的记忆目录，附带最近三个文件的内容。
type MemoryPin struct {
	Path     string       `json:"path"`
	Name     string       `json:"name"`
	Status   string       `json:"status"`
	PinnedAt string       `json:"pinnedAt"`
	Files    []MemoryFile `json:"files"`
}

// MemoryFile 是记忆目录下的一个文件。
type MemoryFile struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
	UpdatedAt   string `json:"updatedAt"`
}

// MemoryImportTask 是导入任务的快照。
type MemoryImportTask struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Status            string   `json:"status"`
	Labels            []string `json:"labels"`
	CreatedAt         string   `json:"createdAt"`
	CompletedAt       string   `json:"completedAt"`
	CompletionSummary string   `json:"completionSummary"`
}

// UploadFile 走 multipart 传到对象存储，返回该版本的确切引用。
//
// 上游限制（实测文档口径）：图片 chat 场景 5 MiB，其它图片 20 MiB，
// 语音 2 MiB，普通文件 50 MiB。这里只做客户端侧的兜底，真实限制由上游判。
func (c *Client) UploadFile(ctx context.Context, filename, mimeType string, data []byte) (*UploadedFile, error) {
	if filename == "" {
		filename = "memory.md"
	}
	if mimeType == "" {
		mimeType = "text/markdown"
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`,
		strings.ReplaceAll(filename, `"`, "")))
	hdr.Set("Content-Type", mimeType)
	part, errPart := mw.CreatePart(hdr)
	if errPart != nil {
		return nil, errPart
	}
	if _, errWrite := part.Write(data); errWrite != nil {
		return nil, errWrite
	}
	if errClose := mw.Close(); errClose != nil {
		return nil, errClose
	}

	status, raw, err := c.doRaw(ctx, http.MethodPost, "/v1/files", mw.FormDataContentType(), &buf)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return nil, classify("memory upload", status, raw)
	}

	var out struct {
		File UploadedFile `json:"file"`
	}
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		return nil, &Error{Kind: KindServer, Msg: "decode uploaded file: " + snippet(raw)}
	}
	if out.File.FileRef.FileID == "" || out.File.FileRef.VersionID == "" {
		return nil, &Error{Kind: KindServer, Msg: "uploaded file missing fileRef: " + snippet(raw)}
	}
	return &out.File, nil
}

// ImportMemory 让上游把已上传的文件导入 Memory。
//
// 语义是「导入」不是「覆写」：上游起一个后台任务，由 agent 抽取内容、
// 决定目标目录并重写格式。同一个 fileRef 重复提交幂等。
func (c *Client) ImportMemory(ctx context.Context, ref FileRef) (*MemoryImportTask, bool, error) {
	return c.importMemory(ctx, ref, "")
}

// ImportMemoryTo 把上传的文件导入到**指定路径**。
//
// target 必须是 `/memory/<...>` 的绝对形态，相对形态上游回
// `400 {"error":"Invalid memory path"}`。目录不存在会自动创建
// （实测 `/memory/verify-deep/sub/newfile.md` 直接写成功）。
//
// 与不带 target 的导入相比，这是「定点写入」：同路径再次导入会
// 先 deleted 旧的再 created 新的（activities 里能直接看到），
// 也就是**替换**而不是叠加。正文原样保留，上游 agent 会在末尾
// 追加一段「来源」说明——这是它自己的行为，无法关闭。
func (c *Client) ImportMemoryTo(ctx context.Context, ref FileRef, target string) (*MemoryImportTask, bool, error) {
	if !strings.HasPrefix(target, "/memory/") || len(target) <= len("/memory/") {
		return nil, false, &Error{Kind: KindServer, Msg: "memory path 必须是 /memory/<...> 形态: " + target}
	}
	return c.importMemory(ctx, ref, target)
}

func (c *Client) importMemory(ctx context.Context, ref FileRef, target string) (*MemoryImportTask, bool, error) {
	body := map[string]any{"fileRef": ref}
	if target != "" {
		body["path"] = target
	}
	status, raw, err := c.doRaw(ctx, http.MethodPost, "/v1/memories/upload", "application/json", jsonReader(body))
	if err != nil {
		return nil, false, err
	}
	if status != http.StatusAccepted && status != http.StatusOK {
		return nil, false, classify("memory import", status, raw)
	}

	var out struct {
		Task       MemoryImportTask `json:"task"`
		Idempotent bool             `json:"idempotent"`
	}
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		return nil, false, &Error{Kind: KindServer, Msg: "decode memory import: " + snippet(raw)}
	}
	if out.Task.ID == "" {
		return nil, false, &Error{Kind: KindServer, Msg: "memory import missing task: " + snippet(raw)}
	}
	return &out.Task, out.Idempotent, nil
}

// MemoryImportTask 取导入任务的最新状态（轮询用）。
//
// 任务详情走 /v1/tasks/{id}；/v2/tasks/{id} 是空的（404），别用。
func (c *Client) MemoryImportTask(ctx context.Context, id string) (*MemoryImportTask, error) {
	if id == "" {
		return nil, &Error{Kind: KindServer, Msg: "empty task id"}
	}
	var out struct {
		Task MemoryImportTask `json:"task"`
	}
	if err := c.getJSON(ctx, "/v1/tasks/"+url.PathEscape(id), nil, "memory task", &out); err != nil {
		return nil, err
	}
	return &out.Task, nil
}

// Pins 列出当前钉住的记忆目录，每个目录带最近三个文件的正文。
//
// 这是本机网络下读记忆的唯一可用入口：DAV 列目录不通，
// 但 pins 返回的内容足够渲染记忆页。
func (c *Client) Pins(ctx context.Context) ([]MemoryPin, error) {
	var out struct {
		Items []MemoryPin `json:"items"`
	}
	if err := c.getJSON(ctx, "/v1/memories/pins", nil, "memory pins", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Pin 钉住一个记忆目录。上游会先经 memory-service 校验路径存在，
// 所以「钉成功」同时也是「这个目录确实存在」的证明。
func (c *Client) Pin(ctx context.Context, path string) (*MemoryPin, error) {
	status, raw, err := c.doRaw(ctx, http.MethodPost, "/v1/memories/pins", "application/json",
		jsonReader(map[string]any{"path": path}))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, classify("memory pin", status, raw)
	}
	var out struct {
		Pin MemoryPin `json:"pin"`
	}
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		return nil, &Error{Kind: KindServer, Msg: "decode memory pin: " + snippet(raw)}
	}
	return &out.Pin, nil
}

// Unpin 取消钉住。
func (c *Client) Unpin(ctx context.Context, path string) error {
	status, raw, err := c.doRaw(ctx, http.MethodDelete, "/v1/memories/pins", "application/json",
		jsonReader(map[string]any{"path": path}))
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return classify("memory unpin", status, raw)
	}
	return nil
}

// AboutUser 读用户画像记忆。
func (c *Client) AboutUser(ctx context.Context) (string, error) {
	var out struct {
		FullContent  string `json:"fullContent"`
		PlainContent string `json:"plainContent"`
	}
	if err := c.getJSON(ctx, "/v1/memories/about-user", nil, "memory about-user", &out); err != nil {
		return "", err
	}
	if out.FullContent != "" {
		return out.FullContent, nil
	}
	return out.PlainContent, nil
}

// ReadMemoryFile 直接读一个记忆文件的正文。
//
// DAV 的文件级 GET 在 /api/cloud 通道上是通的（已实测 200 原文），
// 只有 PROPFIND 列举坏了。所以按已知路径取文件不必绕 pins。
func (c *Client) ReadMemoryFile(ctx context.Context, path string) (string, error) {
	status, raw, err := c.doRaw(ctx, http.MethodGet, davPath(path), "", nil)
	if err != nil {
		return "", err
	}
	if status == http.StatusNotFound {
		return "", &Error{Kind: KindServer, Status: status, Msg: "记忆文件不存在: " + path}
	}
	if status != http.StatusOK {
		return "", classify("memory read", status, raw)
	}
	return string(raw), nil
}

// WriteMemoryFile 精确覆写一个记忆文件，内容一字节不改。
//
// 上游原生支持 PUT /memory/<path>（已实测 201，GET 回读与写入完全一致）。
// 这条路径不经过任何 agent，所以不会像 /v1/memories/upload 那样被重写正文。
// 目录不存在时上游会自动创建。
func (c *Client) WriteMemoryFile(ctx context.Context, path string, data []byte) error {
	contentType := "text/markdown"
	if !strings.HasSuffix(strings.ToLower(path), ".md") {
		contentType = "text/plain"
	}
	status, raw, err := c.doRaw(ctx, http.MethodPut, davPath(path), contentType, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK && status != http.StatusNoContent {
		return classify("memory write", status, raw)
	}
	return nil
}

// DeleteMemoryFile 删除一个记忆文件（上游 DELETE 实测 204）。
func (c *Client) DeleteMemoryFile(ctx context.Context, path string) error {
	status, raw, err := c.doRaw(ctx, http.MethodDelete, davPath(path), "", nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return &Error{Kind: KindServer, Status: status, Msg: "记忆文件不存在: " + path}
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return classify("memory delete", status, raw)
	}
	return nil
}

// davPath 把 /memory/a/b.md 变成上游能吃的 /memory/a/b.md（带前导斜杠、去重斜杠）。
// 传给客户端之前统一校验前缀，避免拿任意路径去打上游。
func davPath(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}

// ValidMemoryPath 校验是不是 /memory/ 下的合法文件路径。
func ValidMemoryPath(p string) bool {
	p = davPath(p)
	return strings.HasPrefix(p, "/memory/") && len(p) > len("/memory/") &&
		!strings.Contains(p, "..")
}

func jsonReader(v any) io.Reader {
	buf, err := json.Marshal(v)
	if err != nil {
		return strings.NewReader("{}")
	}
	return bytes.NewReader(buf)
}
