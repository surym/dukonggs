package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	pb "dukonggs/protocol"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	port           = flag.Int("port", 50051, "The server port")
	gameNameByType = map[string]string{
		GameTypeTyping: "타이핑게임",
		GameTypeCard:   "카드게임",
	}
)

// Game Types
const (
	GameTypeTyping = "typing"
	GameTypeCard   = "card"
)

// Commands for GS -> Clients
const (
	GameStarted = "game started"
	GameCleared = "game cleared"
)

type server struct {
	pb.UnimplementedDukongGSServer

	BroadcastChat                   chan *pb.ChatMessage
	BroadcastCommand                chan *pb.CommandMessage
	ClientNames                     map[string]string
	CommandStreams                  map[string]chan *pb.CommandMessage
	ChatStreams                     map[string]chan *pb.ChatMessage
	namesMtx, commandsMtx, chatsMtx sync.RWMutex
}

func newDukongGS() *server {
	return &server{
		BroadcastCommand: make(chan *pb.CommandMessage),
		BroadcastChat:    make(chan *pb.ChatMessage, 1000),
		ClientNames:      make(map[string]string),
		CommandStreams:   make(map[string]chan *pb.CommandMessage),
		ChatStreams:      make(map[string]chan *pb.ChatMessage),
	}
}

var dukongGS *server = nil

func (s *server) Login(in *pb.LoginRequest, stream pb.DukongGS_LoginServer) error {

	name := in.GetName()
	log.Printf("Login(%s), start command stream ...", name)

	chann := s.openChannelForCommandStream(name)
	defer s.closeChannelForCommandStream(name)

	for {
		select {
		case res := <-chann:
			if s, ok := status.FromError(stream.Send(res)); ok {
				switch s.Code() {
				case codes.OK:
					// success, do nothing
				case codes.Unavailable, codes.Canceled, codes.DeadlineExceeded:
					log.Printf("Login(%s), terminated connection", name)
					break
				default:
					log.Printf("Login(%s), failed to send to client (%s)", name, s.Err())
					break
				}
			}
		}
	}

	return nil
}

func (s *server) openChannelForCommandStream(name string) (stream chan *pb.CommandMessage) {
	chann := make(chan *pb.CommandMessage, 100)

	s.commandsMtx.Lock()
	s.CommandStreams[name] = chann
	s.commandsMtx.Unlock()

	log.Printf("open command stream for client (%s).\n", name)

	return chann
}

func (s *server) closeChannelForCommandStream(name string) {
	s.commandsMtx.Lock()
	if chann, ok := s.CommandStreams[name]; ok {
		delete(s.CommandStreams, name)
		close(chann)
	}
	log.Printf("close command stream for client (%s).\n", name)
	s.commandsMtx.Unlock()
}

func (s *server) StartGame(_ context.Context, in *pb.StartGameRequest) (*pb.StartGameReply, error) {

	if in.GetGameType() != GameTypeTyping {
		return &pb.StartGameReply{Success: false, Session: "empty session", Message: fmt.Sprintf("아직 지원하지 않는 게임타입(%s) 입니다.", in.GetGameType())}, nil
	}

	session, game := getNewGameSession()
	err := SendCommandToClients(
		GameTypeTyping,
		fmt.Sprintf("유저(%s)가 게임(%s)을 요청했습니다.", in.GetName(), gameNameByType[in.GetGameType()]), session)
	if err != nil {
		return &pb.StartGameReply{Success: false, Session: session, Message: fmt.Sprintf("서버 에러가 발생해서 게임을 시작할 수 없습니다. (서버에러:%s)", err)}, nil
	}

	SendCommandToClients(GameTypeTyping, fmt.Sprintf("새 게임이 시작되었습니다! (세션:%s) (시작시간:%s)", game.session, game.startTime), session)
	SendCommandToClients(GameTypeTyping, fmt.Sprintf("미션 스트링: (%s)", game.mission), session)
	SendCommandToClients(GameTypeTyping, GameStarted, session)

	return &pb.StartGameReply{Success: true, Session: session, Message: fmt.Sprintf("새 게임 시작")}, nil
}

func (s *server) StopGame(_ context.Context, in *pb.StopGameRequest) (*pb.StopGameReply, error) {

	exist, game := checkGameSession(in.Session)
	if !exist {
		return &pb.StopGameReply{Success: false, Message: fmt.Sprintf("유효하지 않은 게임 세션입니다. (세션:%s)", in.Session)}, nil
	}

	if strings.Compare(game.mission, in.Content) != 0 {
		return &pb.StopGameReply{Success: false, Message: fmt.Sprintf("응답(%s)과 미션 스트링이 일치하지 않습니다.", in.Content)}, nil
	}

	curr := time.Now()
	diff := curr.Sub(*game.startTime)

	SendCommandToClients(GameTypeTyping, fmt.Sprintf("게임이 종료 되었습니다! (Winner:%s) (세션:%s) (총 소요시간:%s)", in.Name, game.session, diff), game.session)
	SendCommandToClients(GameTypeTyping, GameCleared, game.session)
	removeGameSession(in.Session)

	return &pb.StopGameReply{Success: true, Message: fmt.Sprintf("game ended. winner (%s)", in.Name)}, nil
}

func (s *server) Chat(srv pb.DukongGS_ChatServer) error {

	log.Printf("Chat - srv(%p), contexnt(%p), (%v)", &srv, srv.Context(), srv)

	first := true

	for {
		req, err := srv.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		if first == true {
			first = false
			log.Printf("(%s): started\n", req.GetName())
			go s.sendBroadcasts(req.GetName(), srv)
		}

		s.BroadcastChat <- &pb.ChatMessage{
			Name:    req.GetName(),
			Message: req.GetMessage(),
		}
	}

	<-srv.Context().Done()
	return srv.Context().Err()
}

func (s *server) sendBroadcasts(name string, srv pb.DukongGS_ChatServer) {

	chann := s.openChannelForChatStream(name) // ChatStreams for each client(name)
	defer s.closeChannelForChatStream(name)

	for {
		select {
		case <-srv.Context().Done():
			return
		case res := <-chann:
			if s, ok := status.FromError(srv.Send(res)); ok {
				switch s.Code() {
				case codes.OK:
					// noop
					log.Printf("client (%s) send to msg(%s) vir srv\n", name, res.Message)
				case codes.Unavailable, codes.Canceled, codes.DeadlineExceeded:
					log.Printf("client (%s) terminated connection\n", name)
					return
				default:
					log.Printf("%s - failed to send to client (%s): %v", timeNow(), name, s.Err())
					return
				}
			}
		}
	}
}

func (s *server) openChannelForChatStream(name string) (stream chan *pb.ChatMessage) {
	chann := make(chan *pb.ChatMessage, 100)

	s.chatsMtx.Lock()
	s.ChatStreams[name] = chann
	s.chatsMtx.Unlock()

	log.Printf("opened stream for client (%s).\n", name)

	return chann
}

func (s *server) closeChannelForChatStream(name string) {
	s.chatsMtx.Lock()

	if chann, ok := s.ChatStreams[name]; ok {
		delete(s.ChatStreams, name)
		close(chann)
	}

	log.Printf("closed stream for client (%s).\n", name)

	s.chatsMtx.Unlock()
}

func (s *server) broadcast(_ context.Context) {
	log.Printf("%s - broadcast: started", timeNow())
	for res := range s.BroadcastChat {
		s.chatsMtx.RLock()
		for _, chann := range s.ChatStreams {
			select {
			case chann <- res:
				// noop
			default:
				log.Printf("%s - client stream full, dropping message.\n", timeNow())
			}
		}
		s.chatsMtx.RUnlock()
	}
}

func (s *server) broadcastCommand(_ context.Context) {
	log.Printf("%s - broadcastCommand: started", timeNow())
	for res := range s.BroadcastCommand {
		s.commandsMtx.RLock()
		for _, chann := range s.CommandStreams {
			select {
			case chann <- res:
				// noop
			default:
				log.Printf("%s - client stream full, dropping message.\n", timeNow())
			}
		}
		s.commandsMtx.RUnlock()
	}
}

// SendCommandToClients ...
func SendCommandToClients(gameType, content string, session string) error {
	if dukongGS == nil {
		return fmt.Errorf("dukongGS == nil")
	}

	dukongGS.BroadcastCommand <- &pb.CommandMessage{
		GameType: gameType,
		Session:  session,
		Content:  content,
	}

	return nil
}

// main() ...
func main() {
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	grpcSvr := grpc.NewServer()
	dukongGS = newDukongGS()
	pb.RegisterDukongGSServer(grpcSvr, dukongGS)
	log.Printf("server listening at %v", lis.Addr())
	go dukongGS.broadcastCommand(ctx)
	go dukongGS.broadcast(ctx)
	if err := grpcSvr.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

// @TODO, surym
func timeNow() string {
	return time.Now().String()
}
