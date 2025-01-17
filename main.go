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

    "github.com/gotd/td/telegram"
    "github.com/gotd/td/telegram/auth"
    "github.com/gotd/td/telegram/storage"
    "github.com/gotd/td/tg"
)

// Config 用来解析 config.json
type Config struct {
    APIID       int    `json:"api_id"`
    APIHash     string `json:"api_hash"`
    PhoneNumber string `json:"phone_number"`
    SessionFile string `json:"session_file"`
    VideosDir   string `json:"videos_dir"`
}

// 全局配置
var cfg Config

// 全局 Client
var globalClient *telegram.Client

func main() {
    // 1. 加载配置
    if err := loadConfig("config.json"); err != nil {
        log.Fatalf("加载config.json失败: %v", err)
    }

    // 2. 创建并运行客户端
    err := runClient()
    if err != nil {
        log.Fatalf("runClient出错: %v", err)
    }

    // 3. 简单演示：开启一个 Goroutine 来轮询获取自己聊天中的新消息
    //    （生产环境可用 Updates 机制来替代）
    go pollForUpdates(context.Background())

    // 让主程序阻塞运行
    select {}
}

// loadConfig 读取 config.json
func loadConfig(path string) error {
    data, err := os.ReadFile(path)
    if err != nil {
        return err
    }
    return json.Unmarshal(data, &cfg)
}

// runClient 初始化并登录 Telegram 客户端
func runClient() error {
    // 准备会话存储 - 使用官方提供的 fileStorage
    // 也可自定义 storage 来实现加密或更多逻辑
    sessionStorage := storage.NewFileStorage(cfg.SessionFile)

    // 创建客户端 (构造函数签名: NewClient(appID, appHash int, opts Options))
    client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
        SessionStorage: sessionStorage,
    })
    globalClient = client // 保存到全局

    // 在 client.Run() 内部，底层会建立到 Telegram 的连接，并可进行身份验证
    ctx := context.Background()
    go func() {
        err := client.Run(ctx, func(ctx context.Context) error {
            // 如果已经登录过，这里 client.Auth() 会检测到session，不会重复触发认证
            // 如果未登录，则执行以下流程:
            flow := auth.NewFlow(
                // 只需手机号和验证码
                auth.CodeOnly(cfg.PhoneNumber, auth.SendCodeOptions{}),
                // 若账号有2FA，则可加密码输入
                auth.PasswordAuth(func(ctx context.Context) (string, error) {
                    // 这里写一个简单的输入即可，也可用 config.json 写死
                    fmt.Print("若账号开启2FA, 请输入密码(无则回车): ")
                    var pwd string
                    fmt.Scanln(&pwd)
                    return pwd, nil
                }),
            )
            if err := client.Auth().IfNecessary(ctx, flow); err != nil {
                return fmt.Errorf("登录失败: %w", err)
            }
            log.Println("Telegram 登录成功或已在登录状态")

            // 这里可以做更多初始化操作
            // ...
            return nil
        })
        if err != nil {
            log.Fatalf("client.Run 出错: %v", err)
        }
    }()

    // 注意: client.Run() 是阻塞的调用，但我们放到 goroutine 中运行
    // 可以根据自己需要决定如何结构化：也可在 main 里直接 client.Run(...)

    // 等待几秒看一下是否登录完成
    time.Sleep(5 * time.Second)
    return nil
}

// pollForUpdates 简易轮询示例：每隔10秒检查一次与“自己”的私聊历史消息
func pollForUpdates(ctx context.Context) {
    resolver := tg.NewClient(globalClient)

    // 先获取自己的 UserID
    selfFull, err := resolver.UsersGetFullUser(ctx, &tg.UsersGetFullUserRequest{
        ID: &tg.InputUserSelf{},
    })
    if err != nil {
        log.Printf("获取自己信息失败: %v", err)
        return
    }
    myID := selfFull.User.GetID()

    var lastMsgID int
    for {
        time.Sleep(10 * time.Second)

        // 拉取最新消息
        msgs, err := resolver.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
            Peer:      &tg.InputPeerUser{UserID: myID},
            OffsetID:  0,
            OffsetDate:0,
            AddOffset: 0,
            Limit:     20,
            MaxID:     0,
            MinID:     0,
            Hash:      0,
        })
        if err != nil {
            log.Printf("MessagesGetHistory 出错: %v", err)
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
}

// handleMessage 判断是否含 download 命令并进行频道下载
func handleMessage(ctx context.Context, resolver *tg.Client, m *tg.Message) {
    text := m.GetMessage()
    if text == "" {
        return
    }

    log.Printf("收到消息[%d]: %q\n", m.ID, text)

    if strings.HasPrefix(strings.ToLower(text), "download ") {
        parts := strings.SplitN(text, " ", 2)
        if len(parts) < 2 {
            return
        }
        link := parts[1]
        channelUsername := parseChannelUsername(link)

        // 启动下载
        go func(msgID int) {
            err := downloadChannelVideos(ctx, resolver, channelUsername)
            if err != nil {
                log.Printf("下载[%s]频道视频失败: %v\n", channelUsername, err)
            } else {
                log.Printf("下载[%s]频道视频完成\n", channelUsername)
            }

            // 下载结束后，把消息改为 finished
            // 注意：Telegram有编辑时限(48小时)，若超时则会失败
            newText := "finished"
            _, eErr := resolver.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
                Peer:    &tg.InputPeerUser{UserID: m.PeerID.UserID},
                ID:      msgID, // 注意这里用大写 ID
                Message: newText,
            })
            if eErr != nil {
                log.Printf("编辑消息[%d]失败: %v\n", msgID, eErr)
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

// downloadChannelVideos 从指定频道拉取历史并下载所有视频
func downloadChannelVideos(ctx context.Context, resolver *tg.Client, channelUsername string) error {
    if err := ensureDir(cfg.VideosDir); err != nil {
        return fmt.Errorf("创建视频目录失败: %w", err)
    }

    // 解析频道用户名 -> channel id
    r, err := resolver.ContactsResolveUsername(ctx, channelUsername)
    if err != nil {
        return fmt.Errorf("解析频道[%s]失败: %w", channelUsername, err)
    }
    if len(r.Chats) == 0 {
        return fmt.Errorf("未找到频道: %s", channelUsername)
    }

    ch, ok := r.Chats[0].(*tg.Channel)
    if !ok {
        return fmt.Errorf("这不是一个Channel: %s", channelUsername)
    }

    inputPeerChannel := &tg.InputPeerChannel{
        ChannelID:  ch.ID,
        AccessHash: ch.AccessHash,
    }

    offsetID := 0
    limit := 50

    for {
        msgs, err := resolver.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
            Peer:       inputPeerChannel,
            OffsetID:   offsetID,
            OffsetDate: 0,
            AddOffset:  0,
            Limit:      limit,
            MaxID:      0,
            MinID:      0,
            Hash:       0,
        })
        if err != nil {
            return fmt.Errorf("MessagesGetHistory 出错: %w", err)
        }
        channelMsgs, ok := msgs.(*tg.MessagesChannelMessages)
        if !ok || len(channelMsgs.Messages) == 0 {
            break
        }

        // 从最新往更早翻
        for i := len(channelMsgs.Messages) - 1; i >= 0; i-- {
            m, _ := channelMsgs.Messages[i].(*tg.Message)
            if m == nil || m.Media == nil {
                continue
            }
            if media, ok := m.Media.(*tg.MessageMediaDocument); ok {
                doc, ok := media.Document.AsNotEmpty()
                if ok && isVideoDocument(doc) {
                    // 下载
                    if err := downloadOneVideo(ctx, doc); err != nil {
                        log.Printf("[msgID=%d] 视频下载失败: %v", m.ID, err)
                    } else {
                        log.Printf("[msgID=%d] 视频下载完成", m.ID)
                    }
                    // 随机等待5~10分钟
                    waitRandomDuration(5, 10)
                }
            }
        }

        // 更新offsetID，继续向更早的消息翻
        oldestID := channelMsgs.Messages[len(channelMsgs.Messages)-1].(*tg.Message).ID
        offsetID = oldestID
        if offsetID == 0 {
            break
        }
    }
    return nil
}

// isVideoDocument 判断是否为视频文件
func isVideoDocument(doc *tg.Document) bool {
    if strings.HasPrefix(doc.MimeType, "video/") {
        return true
    }
    for _, attr := range doc.Attributes {
        if _, ok := attr.(*tg.DocumentAttributeVideo); ok {
            return true
        }
    }
    return false
}

// downloadOneVideo 下载单个视频到 cfg.VideosDir
func downloadOneVideo(ctx context.Context, doc *tg.Document) error {
    fileName := "video"
    for _, attr := range doc.Attributes {
        if fn, ok := attr.(*tg.DocumentAttributeFilename); ok {
            fileName = fn.FileName
        }
    }
    outputPath := filepath.Join(cfg.VideosDir, fileName)

    f, err := os.Create(outputPath)
    if err != nil {
        return err
    }
    defer f.Close()

    // 分块下载
    blockSize := 64 * 1024
    var offset int64

    for {
        data, err := globalClient.API().UploadGetFile(ctx, &tg.UploadGetFileRequest{
            Location: &tg.InputDocumentFileLocation{
                ID:            doc.ID,
                AccessHash:    doc.AccessHash,
                FileReference: doc.FileReference,
                ThumbSize:     "",
            },
            Offset: int(offset),
            Limit:  blockSize,
        })
        if err != nil {
            return err
        }

        fileChunk, ok := data.(*tg.UploadFile)
        if !ok {
            return fmt.Errorf("返回类型错误")
        }
        if len(fileChunk.Bytes) == 0 {
            break
        }

        if _, err := f.Write(fileChunk.Bytes); err != nil {
            return err
        }
        offset += int64(len(fileChunk.Bytes))

        if len(fileChunk.Bytes) < blockSize {
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
    log.Printf("等待 %d 分钟再下载下一个视频...", delay)
    time.Sleep(time.Duration(delay) * time.Minute)
}

// ensureDir 确保目录存在
func ensureDir(path string) error {
    if _, err := os.Stat(path); os.IsNotExist(err) {
        return os.MkdirAll(path, 0755)
    }
    return nil
}
