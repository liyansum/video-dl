package main

import (
    "context"
    "crypto/rand"
    "encoding/json"
    "fmt"
    "log"
    "math/big"
    "os"
    "path/filepath"
    "strings"
    "time"

    "github.com/gotd/td"
    "github.com/gotd/td/tg"
    "github.com/gotd/td/telegram"
)

// Config 用于解析 config.json
type Config struct {
    APIID       int    `json:"api_id"`
    APIHash     string `json:"api_hash"`
    PhoneNumber string `json:"phone_number"`
    SessionFile string `json:"session_file"`
    VideosDir   string `json:"videos_dir"`
}

// 全局客户端
var globalClient *telegram.Client

// 全局配置
var cfg Config

func main() {
    // 1. 加载 config.json
    c, err := loadConfig("config.json")
    if err != nil {
        log.Fatalf("加载配置文件出错: %v\n", err)
    }
    cfg = *c // 保存到全局

    // 2. 启动并登录客户端
    err = runClient()
    if err != nil {
        log.Fatalf("运行/登录客户端出错: %v\n", err)
    }
    defer globalClient.Close()

    // 3. 启动一个 goroutine 轮询获取“自己”对话里的新消息
    ctx := context.Background()
    pollForUpdates(ctx)

    // 4. 阻塞主线程或做其他事情
    select {}
}

// runClient 启动并登录客户端
func runClient() error {
    // 创建一个 session 文件存储对象
    storage := &fileSessionStorage{filePath: cfg.SessionFile}

    // 创建一个 Telegram 客户端
    client := telegram.NewClient(td.NewSessionStorage(), telegram.Options{
        APIID:          cfg.APIID,
        APIHash:        cfg.APIHash,
        SessionStorage: storage,
    })
    globalClient = client

    // 启动并登录
    ctx := context.Background()
    return client.Run(ctx, func(ctx context.Context) error {
        // 若 session 文件存在，则无需重新登录
        if storage.hasSession() {
            fmt.Println("检测到已有 session，无需重新登录。")
            return nil
        }

        // 否则需要进行手机号+验证码+2FA 登录
        fmt.Println("首次登录，进行手机号、验证码验证。")
        authFlow := telegram.CodeAuth(
            // 请求手机号
            func(ctx context.Context) (string, error) {
                return cfg.PhoneNumber, nil
            },
            // 请求验证码
            func(ctx context.Context, codeSent *telegram.CodeResponse) (string, error) {
                var code string
                fmt.Printf("请输入发送到 [%s] 的验证码: ", cfg.PhoneNumber)
                fmt.Scanln(&code)
                return code, nil
            },
            // 如果账户开启了2FA，需要输入密码
            func(ctx context.Context) (string, error) {
                fmt.Printf("请输入 2FA 密码(若无则直接回车): ")
                var pwd string
                fmt.Scanln(&pwd)
                return pwd, nil
            },
        )
        if err := client.Auth(ctx, authFlow); err != nil {
            return fmt.Errorf("登录失败: %w", err)
        }
        fmt.Println("登录成功，Session 已保存。")
        return nil
    })
}

// pollForUpdates 轮询获取“自己”对话的新消息(示例)
func pollForUpdates(ctx context.Context) {
    go func() {
        resolver := tg.NewClient(globalClient)

        // 先获取自己的用户信息
        self, err := resolver.UsersGetFullUser(ctx, &tg.UsersGetFullUserRequest{
            Id: &tg.InputUserSelf{},
        })
        if err != nil {
            log.Printf("获取自己用户信息失败: %v\n", err)
            return
        }
        myID := self.User.GetID()

        var lastMsgID int
        for {
            // 每隔10秒获取一次消息
            time.Sleep(10 * time.Second)

            msgs, err := resolver.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
                Peer:      &tg.InputPeerUser{UserID: myID},
                OffsetId:  0,
                OffsetDate:0,
                AddOffset: 0,
                Limit:     20,
                MaxId:     0,
                MinId:     0,
                Hash:      0,
            })
            if err != nil {
                log.Printf("MessagesGetHistory 出错: %v\n", err)
                continue
            }

            msgContainer, ok := msgs.(*tg.MessagesMessages)
            if !ok {
                continue
            }

            // 找出新的消息
            var newMessages []*tg.Message
            for i := len(msgContainer.Messages) - 1; i >= 0; i-- {
                m, ok := msgContainer.Messages[i].(*tg.Message)
                if !ok {
                    continue
                }
                // 只处理ID更大的(更新的)
                if m.ID > lastMsgID {
                    newMessages = append(newMessages, m)
                }
            }

            // 处理新的消息
            for _, m := range newMessages {
                handleMessage(ctx, resolver, m)
                if m.ID > lastMsgID {
                    lastMsgID = m.ID
                }
            }
        }
    }()
}

// handleMessage 根据消息内容执行下载等操作
func handleMessage(ctx context.Context, resolver *tg.Client, m *tg.Message) {
    text := m.GetMessage()
    if text == "" {
        return
    }
    log.Printf("收到消息: %q\n", text)

    // 判断是否包含 download 命令
    // 例如: download https://t.me/xxxx
    if strings.HasPrefix(strings.ToLower(text), "download ") {
        parts := strings.SplitN(text, " ", 2)
        if len(parts) < 2 {
            return
        }
        link := parts[1]
        channelUsername := parseChannelUsername(link)

        // 异步下载，避免阻塞
        go func(msgID int) {
            err := downloadChannelVideos(ctx, resolver, channelUsername)
            if err != nil {
                log.Printf("下载频道[%s] 视频出错: %v\n", channelUsername, err)
            } else {
                log.Printf("频道[%s] 视频全部下载完成\n", channelUsername)
            }

            // 下载完成后，将“download ...”消息改为 "finished"
            // 注意时限和权限(如果超过 Telegram 编辑时间限制, 可能失败)
            newText := "finished"
            editReq := &tg.MessagesEditMessageRequest{
                Peer:    &tg.InputPeerUser{UserID: m.PeerID.UserID},
                Id:      msgID,
                Message: newText,
            }
            if _, err := resolver.MessagesEditMessage(ctx, editReq); err != nil {
                log.Printf("编辑消息失败: %v\n", err)
            }
        }(m.ID)
    }
}

// parseChannelUsername t.me/xxxx -> xxxx
func parseChannelUsername(link string) string {
    link = strings.TrimSpace(link)
    link = strings.TrimPrefix(link, "https://t.me/")
    link = strings.TrimPrefix(link, "http://t.me/")
    link = strings.TrimPrefix(link, "t.me/")
    return link
}

// downloadChannelVideos 遍历频道消息并下载所有视频
func downloadChannelVideos(ctx context.Context, resolver *tg.Client, channelUsername string) error {
    if err := ensureDir(cfg.VideosDir); err != nil {
        return fmt.Errorf("创建视频目录失败: %w", err)
    }

    // 1. 解析 channel username -> channel id
    r, err := resolver.ContactsResolveUsername(ctx, channelUsername)
    if err != nil {
        return fmt.Errorf("解析频道username失败: %w", err)
    }
    if len(r.Chats) == 0 {
        return fmt.Errorf("未找到频道: %s", channelUsername)
    }

    var inputPeerChannel *tg.InputPeerChannel
    switch ch := r.Chats[0].(type) {
    case *tg.Channel:
        inputPeerChannel = &tg.InputPeerChannel{
            ChannelID:  ch.ID,
            AccessHash: ch.AccessHash,
        }
    case *tg.Chat:
        return fmt.Errorf("这是一个普通 Chat 而非 Channel: %s", channelUsername)
    default:
        return fmt.Errorf("未知 chat 类型")
    }

    offsetID := 0
    limit := 50

    for {
        // 获取一批历史消息
        msgs, err := resolver.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
            Peer:       inputPeerChannel,
            OffsetId:   offsetID,
            OffsetDate: 0,
            AddOffset:  0,
            Limit:      limit,
            MaxId:      0,
            MinId:      0,
            Hash:       0,
        })
        if err != nil {
            return fmt.Errorf("获取频道历史消息失败: %w", err)
        }

        channelMsgs, ok := msgs.(*tg.MessagesChannelMessages)
        if !ok || len(channelMsgs.Messages) == 0 {
            // 没有更多消息了
            break
        }

        // 从最新翻到更早(可以自行调整顺序)
        for i := len(channelMsgs.Messages) - 1; i >= 0; i-- {
            msg, ok := channelMsgs.Messages[i].(*tg.Message)
            if !ok || msg.Media == nil {
                continue
            }
            // 判断是否含有视频
            switch media := msg.Media.(type) {
            case *tg.MessageMediaDocument:
                doc, ok := media.Document.AsNotEmpty()
                if !ok {
                    continue
                }
                if isVideoDocument(doc) {
                    // 下载
                    err := downloadOneVideo(ctx, doc)
                    if err != nil {
                        log.Printf("[msgID=%d] 下载视频出错: %v", msg.ID, err)
                    } else {
                        log.Printf("[msgID=%d] 视频下载完成", msg.ID)
                    }
                    // 下载完一个后, 随机等待 5~10 分钟
                    waitRandomDuration(5, 10)
                }
            }
        }

        // 更新 offsetID (取当前批次中最早一条消息的ID)
        oldestID := channelMsgs.Messages[len(channelMsgs.Messages)-1].(*tg.Message).ID
        offsetID = oldestID
        if offsetID == 0 {
            break
        }
    }

    return nil
}

// isVideoDocument 判断 Document 是否是视频
func isVideoDocument(doc *tg.Document) bool {
    // 判断 mimeType
    if strings.HasPrefix(doc.MimeType, "video/") {
        return true
    }
    // 或者判断属性里是否存在 DocumentAttributeVideo
    for _, attr := range doc.Attributes {
        if _, ok := attr.(*tg.DocumentAttributeVideo); ok {
            return true
        }
    }
    return false
}

// downloadOneVideo 下载单个视频到 cfg.VideosDir
func downloadOneVideo(ctx context.Context, doc *tg.Document) error {
    // 确定文件名
    fileName := "video"
    for _, attr := range doc.Attributes {
        switch a := attr.(type) {
        case *tg.DocumentAttributeFilename:
            fileName = a.FileName
        }
    }

    outputPath := filepath.Join(cfg.VideosDir, fileName)

    // 如果文件已存在，可根据需求决定是否跳过或覆盖
    // 这里简单覆盖
    f, err := os.Create(outputPath)
    if err != nil {
        return fmt.Errorf("创建文件失败: %w", err)
    }
    defer f.Close()

    // 使用 UploadGetFile 分块下载 (每块64KB)
    blockSize := 64 * 1024
    offset := int64(0)

    for {
        data, err := globalClient.API().UploadGetFile(ctx, &tg.UploadGetFileRequest{
            Location: &tg.InputDocumentFileLocation{
                Id:            doc.ID,
                AccessHash:    doc.AccessHash,
                FileReference: doc.FileReference,
                ThumbSize:     "",
            },
            Offset: int(offset),
            Limit:  blockSize,
        })
        if err != nil {
            return fmt.Errorf("UploadGetFile 出错: %w", err)
        }

        file, ok := data.(*tg.UploadFile)
        if !ok {
            return fmt.Errorf("返回结果不是 UploadFile")
        }

        if len(file.Bytes) == 0 {
            // 下载结束
            break
        }

        // 写文件
        if _, werr := f.Write(file.Bytes); werr != nil {
            return fmt.Errorf("写文件失败: %w", werr)
        }

        offset += int64(len(file.Bytes))
        if len(file.Bytes) < blockSize {
            // 最后一块
            break
        }
    }
    return nil
}

// waitRandomDuration 在 [min, max] 分钟之间随机等待
func waitRandomDuration(min, max int64) {
    n, _ := rand.Int(rand.Reader, big.NewInt(max-min+1))
    delay := n.Int64() + min
    log.Printf("等待 %d 分钟后再下载下一个视频...\n", delay)
    time.Sleep(time.Duration(delay) * time.Minute)
}

// ensureDir 确保目录存在
func ensureDir(path string) error {
    if _, err := os.Stat(path); os.IsNotExist(err) {
        return os.MkdirAll(path, 0755)
    }
    return nil
}

// ---------------------
// fileSessionStorage 用于管理 session 文件

type fileSessionStorage struct {
    filePath string
    session  []byte
}

// LoadSession 实现 telegram.SessionLoader
func (fs *fileSessionStorage) LoadSession(ctx context.Context) ([]byte, error) {
    data, err := os.ReadFile(fs.filePath)
    if err != nil {
        if os.IsNotExist(err) {
            return nil, telegram.ErrSessionNotFound
        }
        return nil, err
    }
    fs.session = data
    return data, nil
}

// StoreSession 实现 telegram.SessionSaver
func (fs *fileSessionStorage) StoreSession(ctx context.Context, data []byte) error {
    fs.session = data
    return os.WriteFile(fs.filePath, data, 0600)
}

// hasSession 判断 session 文件是否存在
func (fs *fileSessionStorage) hasSession() bool {
    if _, err := os.Stat(fs.filePath); err == nil {
        return true
    }
    return false
}
