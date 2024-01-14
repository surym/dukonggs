/*
 *
 * Copyright 2015 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package main implements a client for DukongGS service.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	pb "dukonggs/protocol"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultName = "world"
)

var (
	addr = flag.String("addr", "localhost:50051", "the address to connect to")
	name = flag.String("name", defaultName, "Name to greet")
)

type GameSession struct {
	underGame bool
	session   string
}

var gameSession GameSession
var gameSessionMtx sync.RWMutex

func gameSessionInitialize() {
	gameSessionMtx.Lock()
	gameSession.session = ""
	gameSession.underGame = false
	gameSessionMtx.Unlock()
}

func setupGameSession(session string, status bool) {
	gameSessionMtx.Lock()
	gameSession.session = session
	gameSession.underGame = status
	gameSessionMtx.Unlock()
}

// func TestDukongGS(t *testing.T) {
func main() {
	flag.Parse()
	gameSessionInitialize()

	// Set up a connection to the server.
	conn, err := grpc.Dial(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()
	c := pb.NewDukongGSClient(conn)

	// Login
	commandStream, err := c.Login(context.Background(), &pb.LoginRequest{Name: *name})
	if err != nil {
		log.Fatalf("Failed to login the chat: %v", err)
	}
	go rcvServerCommands(commandStream)

	chatStream, err := c.Chat(context.Background())
	if err != nil {
		log.Fatalf("Failed to join the chat: %v", err)
	}
	err = chatStream.Send(&pb.ChatMessage{Name: *name, Message: "join"})
	if err != nil {
		log.Fatalf("Failed to send message: %s", err)
	}
	log.Printf("Joined")

	defer chatStream.CloseSend()
	go rcvChatMessages(chatStream)
	for {
		fmt.Printf(": ")
		message, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		message = strings.TrimRight(message, "\r\n")
		message = strings.TrimLeft(message, "\r\n")

		if strings.Compare(message, "start game") == 0 {
			gameStartResp, err := c.StartGame(context.Background(), &pb.StartGameRequest{GameType: "typing", Name: *name})
			if err != nil {
				log.Printf("StartGame failed (%s)", err)
				setupGameSession("", false)
				continue
			}
			if gameStartResp.Success != true {
				log.Printf("StartGameResp.Success != true (%s)", gameStartResp.GetMessage())
				continue
			}

			session := gameStartResp.GetSession()
			setupGameSession(session, true)
			continue

		} else if strings.Compare(message, "stop game") == 0 {
			log.Printf("forced game stop")
			setupGameSession("", false)

		} else if gameSession.underGame == true {
			gameStopResp, err := c.StopGame(
				context.Background(),
				&pb.StopGameRequest{Name: *name, GameType: "typing", Session: gameSession.session, Content: message})
			if err != nil {
				log.Printf("StartGame failed (%s)", err)
				continue
			}
			if gameStopResp.Success != true {
				log.Printf("gameStopResp.Success != true (%s)", gameStopResp.GetMessage())
				continue
			}

			// win!
			log.Printf("gameStopResp.Succes == true ")
			setupGameSession("", false)
			continue
		}

		err = chatStream.Send(&pb.ChatMessage{Name: *name, Message: message})
		if err != nil {
			log.Fatalf("Failed to send message: %s", err)
		}
	}
}

func rcvServerCommands(stream pb.DukongGS_LoginClient) {
	log.Printf("rcvServerCommands(DukongGS_LoginClient) ready...\n")
	defer stream.CloseSend()

	for {
		message, err := stream.Recv()
		if err == io.EOF {
			log.Printf("rcvServerCommands: err == io.EOF, break")
			time.Sleep(time.Second)
			continue
		}
		if err != nil {
			log.Fatalf("Error receiving command: %v", err)
		}
		content := message.GetContent()
		log.Printf("rcvServerCommands - Received!! (%s: %s)\n", message.GameType, content)

		if strings.Compare(content, "game started") == 0 {
			log.Printf("game(%s) started - session:(%s)\n", message.GameType, message.Session)
			setupGameSession(message.Session, true)
		} else if strings.Compare(content, "game cleared") == 0 {
			log.Printf("game(%s) cleared - session:(%s)\n", message.GameType, message.Session)
			setupGameSession("", false)
		}
	}
}

func rcvChatMessages(stream pb.DukongGS_ChatClient) {
	log.Printf("rcvChatMessages(DukongGS_ChatClient) ready...\n")
	for {
		message, err := stream.Recv()
		if err == io.EOF {
			log.Printf("rcvChatMessages: err == io.EOF, break")
			break
		}
		if err != nil {
			log.Fatalf("Error receiving message: %v", err)
		}
		log.Printf("%s: %s", message.GetName(), message.GetMessage())
		fmt.Printf(": ")
	}
}
