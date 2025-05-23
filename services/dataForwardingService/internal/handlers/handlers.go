package handlers

import (
	pb "Betterfly2/proto/data_forwarding"
	"Betterfly2/shared/logger"
	"data_forwarding_service/internal/publisher"
	"data_forwarding_service/internal/redis"
	"fmt"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
	"net/http"
	"os"
	"strconv"
	"sync"
)

// Client 连接管理
type Client struct {
	conn       *websocket.Conn
	sendChan   chan []byte
	shouldStop bool // 当shouldStop为true时，读、写协程立刻退出工作
	loggedIn   bool // 是否已登录
}

// 用于存储 WebSocket 连接的map
var (
	clients      = make(map[string]*Client) // {(用户ID: 客户端)
	clientsMutex sync.Mutex                 // 互斥锁
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// StartWebSocketServer 启动WebSocket服务器
func StartWebSocketServer() error {
	http.HandleFunc("/ws", handleConnection)
	port := os.Getenv("PORT")
	if port == "" {
		port = "54342"
	}

	certFile := os.Getenv("CERT_PATH")
	if certFile == "" {
		certFile = "./certs/cert.pem"
	}

	keyFile := os.Getenv("KEY_PATH")
	if keyFile == "" {
		keyFile = "./certs/key.pem"
	}

	return http.ListenAndServeTLS(":"+port, certFile, keyFile, nil)
}

// 请求处理
func handleConnection(w http.ResponseWriter, r *http.Request) {
	sugar := logger.Sugar()
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		sugar.Errorf("连接错误: %s", err)
		return
	}

	// 连接时用ip:port临时作为键
	userID := conn.RemoteAddr().String()

	client := &Client{
		conn:       conn,
		sendChan:   make(chan []byte, 256),
		shouldStop: false,
		loggedIn:   false,
	}

	// 未登录时直接保存
	clientsMutex.Lock()
	clients[userID] = client
	clientsMutex.Unlock()

	sugar.Infof("已与 %v 建立连接", conn.RemoteAddr())
	sugar.Infof("收到的Request内容为: %v", *r)

	// 启动两个 goroutine
	go readProcess(client, userID)
	go writeToClient(client, userID)
}

// 读取处理协程
func readProcess(client *Client, userID string) {
	sugar := logger.Sugar()
	defer func() {
		clientsMutex.Lock()
		delete(clients, userID)
		clientsMutex.Unlock()
		client.conn.Close()

		// 如果已登录才会在redis中注册
		if client.loggedIn {
			containerID := os.Getenv("HOSTNAME")
			if containerID == "" {
				containerID = "message-topic"
			}
			redisClient.UnregisterConnection(userID, containerID)
		}

		sugar.Infof("(%v, %v)连接已关闭", userID, client.conn.RemoteAddr())
	}()

	for {
		// 处理消息接收与转发
		_, p, err := client.conn.ReadMessage()

		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				sugar.Infof("连接关闭，读协程退出")
			} else {
				sugar.Errorln("获取信息异常: ", err)
			}
			close(client.sendChan)
			break
		}

		if len(p) == 0 {
			continue
		}

		requestMsg, err := HandleRequestData(p)
		if err != nil {
			sugar.Warnf("收到非标准化数据: %v", err)
			continue
		}

		// 如果未登录，只处理三种报文
		if !client.loggedIn {
			switch requestMsg.Payload.(type) {
			case *pb.RequestMessage_Login:
				rsp, realUserID, err := HandleLoginMessage(requestMsg)
				logger.Sugar().Infof("rsp: %s", rsp.String())
				if err != nil {
					logger.Sugar().Errorf("登录出现错误: %v", err)
					rspBytes, _ := proto.Marshal(rsp)
					client.sendChan <- rspBytes
					continue
				}
				oldUserID := userID
				userID = strconv.FormatInt(realUserID, 10)
				err = checkAndResolveConflict(userID, client)
				if err != nil {
					logger.Sugar().Errorf("登录解决冲突失败: %v", err)
				} else {
					// 删除旧键值对
					clientsMutex.Lock()
					delete(clients, oldUserID)
					clientsMutex.Unlock()
					client.loggedIn = true
				}
				// 返回登录结果
				rspBytes, _ := proto.Marshal(rsp)
				client.sendChan <- rspBytes
			case *pb.RequestMessage_Signup:
				rsp, err := HandleSignupMessage(requestMsg)
				logger.Sugar().Infof("rsp: %s", rsp.String())
				if err != nil {
					logger.Sugar().Errorf("注册出现错误：: %v", err)
				}
				rspBytes, _ := proto.Marshal(rsp)
				client.sendChan <- rspBytes
			case *pb.RequestMessage_Logout:
				// 终止掉当前连接
				break
			default:
				logger.Sugar().Errorln("未登录时不处理其他类型信息")
				rsp := &pb.ResponseMessage{
					Payload: &pb.ResponseMessage_Refused{},
				}
				rspBytes, _ := proto.Marshal(rsp)
				client.sendChan <- rspBytes
			}
		} else {
			intUserID, err := strconv.ParseInt(userID, 10, 64)
			if err != nil {
				logger.Sugar().Errorf("无法将 %s 转为int64: %v", userID, err)
				continue
			}
			res, err := RequestMessageHandler(intUserID, requestMsg)
			if err != nil {
				logger.Sugar().Errorf("消息处理错误: %v", err)
			}
			if res == 1 {
				// res为1代表后续收到logout报文，需要断开连接
				break
			}
		}
		// TODO: DEBUG模式
		sugar.Infoln("收到WebSocket消息:", string(p))
	}
}

// 监听 channel 发送消息协程
func writeToClient(client *Client, userID string) {
	sugar := logger.Sugar()
	defer func() {
		sugar.Infof("连接关闭，写协程退出")
	}()
	for msg := range client.sendChan {
		err := client.conn.WriteMessage(websocket.BinaryMessage, msg)
		if err != nil {
			sugar.Errorln("发送消息错误: ", err)
		}
	}
}

// 调用消息队列发布接口完成消息发布
func publishMessage(message []byte, targetTopic string) error {
	return publisher.PublishMessage(string(message), targetTopic)
}

// SendMessage 外部发送消息接口
func SendMessage(userID string, message []byte) error {
	clientsMutex.Lock()
	client, ok := clients[userID]
	clientsMutex.Unlock()
	if !ok {
		return fmt.Errorf("客户端%v不存在", userID)
	}

	// 通过 channel 发送消息
	client.sendChan <- message
	return nil
}

// StopClient 外部关闭特定连接
func StopClient(userID string) {
	clientsMutex.Lock()
	client, ok := clients[userID]
	clientsMutex.Unlock()
	if !ok {
		return
	}
	client.conn.Close()
	client.shouldStop = true
}

// checkAndResolveConflict 检验并解决连接冲突
func checkAndResolveConflict(userID string, client *Client) error {
	sugar := logger.Sugar()

	containerID := os.Getenv("HOSTNAME")
	if containerID == "" {
		containerID = "message-topic"
	}

	// 第一步：清理本地已有连接
	clientsMutex.Lock()
	if oldClient, ok := clients[userID]; ok {
		sugar.Infof("已有本地连接，关闭旧连接: %v", userID)
		oldClient.conn.Close()
		delete(clients, userID)
		if err := redisClient.UnregisterConnection(userID, containerID); err != nil {
			sugar.Warnf("本地Redis注销失败（忽略继续）: %v", err)
		}
	}
	clientsMutex.Unlock()

	// 第二步：检测是否远程已注册
	remoteContainer := redisClient.GetContainerByConnection(userID)
	sugar.Infof("远程容器: %v", remoteContainer)

	if remoteContainer != "" && remoteContainer != containerID {
		sugar.Infof("用户 %s 存在于其他容器 %s", userID, remoteContainer)

		// 注销旧连接
		if err := redisClient.UnregisterConnection(userID, remoteContainer); err != nil {
			return fmt.Errorf("注销 Redis 失败: %w", err)
		}

		// 通知旧容器断开连接
		if err := publishMessage([]byte(fmt.Sprintf("DELETE USER %s", userID)), remoteContainer); err != nil {
			return fmt.Errorf("通知远程容器失败: %w", err)
		}
	}

	// 第三步：注册本连接
	if err := redisClient.RegisterConnection(userID, containerID); err != nil {
		return fmt.Errorf("注册 Redis 失败: %w", err)
	}

	// 第四步：保存本地连接
	clientsMutex.Lock()
	clients[userID] = client
	clientsMutex.Unlock()

	sugar.Infof("连接 %s 注册并保存成功", userID)
	return nil
}
