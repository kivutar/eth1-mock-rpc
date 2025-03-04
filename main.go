package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/prysmaticlabs/eth1-mock-rpc/eth1"
	"github.com/sirupsen/logrus"
	prefixed "github.com/x-cray/logrus-prefixed-formatter"
	"golang.org/x/net/websocket"
)

const (
	maxRequestContentLength = 1024 * 512
	defaultErrorCode        = -32000
)

var (
	keystorePath          = flag.String("keystore-path", "", "Path to a validator keystore directory")
	password              = flag.String("password", "", "Password to unlocking the validator keystore directory")
	wsPort                = flag.String("ws-port", "7778", "Port on which to serve websocket listeners")
	httpPort              = flag.String("http-port", "7777", "Port on which to serve http listeners")
	invalidateCache       = flag.Bool("invalidate-cache", false, "Recalculate deposits into a cache from a keystore")
	numGenesisDeposits    = flag.Int("genesis-deposits", 0, "Number of deposits to read from the keystore to trigger the genesis event")
	verbosity             = flag.String("verbosity", "info", "Logging verbosity (debug, info=default, warn, error, fatal, panic)")
	log                   = logrus.WithField("prefix", "main")
	persistedDepositsJSON = "deposits.json"
)

type server struct {
	depositsLock           sync.Mutex
	numDepositsReadyToSend int
	deposits               []*eth1.DepositData
	eth1Logs               []types.Log
	genesisTime            uint64
}

type websocketHandler struct {
	blockNum      uint64
	close         chan bool
	readOperation chan []*jsonrpcMessage // Channel for read messages from the codec.
	readErr       chan error
}

func main() {
	flag.Parse()
	formatter := new(prefixed.TextFormatter)
	formatter.TimestampFormat = "2006-01-02 15:04:05"
	formatter.FullTimestamp = true
	logrus.SetFormatter(formatter)
	level, err := logrus.ParseLevel(*verbosity)
	if err != nil {
		log.Fatal(err)
	}
	logrus.SetLevel(level)

	if *numGenesisDeposits == 0 {
		log.Fatal("Please enter a valid number of --genesis-deposits to read from the keystore")
	}

	var allDeposits []*eth1.DepositData
	tmp := os.TempDir()
	cachePath := path.Join(tmp, persistedDepositsJSON)
	recalculate := *invalidateCache
	// We attempt to retrieve deposits from a local tmp file
	// as an optimization to prevent reading and decrypting raw private keys
	// from the validator keystore every single time the mock server is launched.
	if r, err := os.Open(cachePath); !recalculate && err == nil {
		allDeposits, err = retrieveDepositData(r)
		if err != nil {
			log.Fatalf("Could not retrieve deposits from %s: %v", cachePath, err)
		}
	} else if recalculate || os.IsNotExist(err) {
		// If the file does not exist at the tmp directory, we decrypt
		// from the keystore directory and then attempt to persist to the cache.
		log.Infof("Decrypting private keys from %s, this may take a while...", *keystorePath)
		allDeposits, err = createDepositDataFromKeystore(*keystorePath, *password)
		if err != nil {
			log.Fatalf("Could not create deposit data from keystore directory: %v", err)
		}
		w, err := os.Create(cachePath)
		if err != nil {
			log.Fatal(err)
		}
		if err := persistDepositData(w, allDeposits); err != nil {
			log.Errorf("Could not persist deposits to disk: %v", err)
		}
	} else {
		log.Fatalf("Could not read from %s: %v", cachePath, err)
	}
	log.Infof("Successfully loaded %d private keys from the keystore directory", len(allDeposits))

	if *numGenesisDeposits > len(allDeposits) {
		log.Fatalf(
			"Number of --genesis-deposits %d > number of deposits found in keystore directory %d",
			*numGenesisDeposits,
			allDeposits,
		)
	}

	httpListener, err := net.Listen("tcp", fmt.Sprintf("localhost:%s", *httpPort))
	if err != nil {
		log.Fatal(err)
	}
	wsListener, err := net.Listen("tcp", fmt.Sprintf("localhost:%s", *wsPort))
	if err != nil {
		log.Fatal(err)
	}
	logs, err := eth1.DepositEventLogs(allDeposits)
	if err != nil {
		log.Fatal(err)
	}
	srv := &server{
		numDepositsReadyToSend: *numGenesisDeposits,
		deposits:               allDeposits,
		eth1Logs:               logs,
		genesisTime:            uint64(time.Now().Add(10 * time.Second).Unix()),
	}
	log.Println("Starting HTTP listener on port :7777")
	go http.Serve(httpListener, srv)

	log.Println("Starting WebSocket listener on port :7778")
	wsSrv := &http.Server{Handler: srv.ServeWebsocket()}
	go wsSrv.Serve(wsListener)

	go srv.listenForDepositTrigger()

	select {}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	body := io.LimitReader(r.Body, maxRequestContentLength)
	conn := &httpServerConn{Reader: body, Writer: w, r: r}
	codec := NewJSONCodec(conn)
	defer codec.Close()
	msgs, _, err := codec.Read()
	if err != nil {
		log.WithError(err).Error("Could not read data from request")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	requestItem := msgs[0]
	if !requestItem.isCall() {
		log.WithField("messageType", requestItem.Method).Error("Can only serve RPC call types via HTTP")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	log.WithField("method", requestItem.Method).Debug("Received HTTP-RPC request")
	log.Debugf("%v", requestItem)

	stringRep := requestItem.String()
	switch requestItem.Method {
	case "eth_getBlockByNumber":
		block := eth1.BlockHeaderByNumber()
		response := requestItem.response(block)
		if err := codec.Write(ctx, response); err != nil {
			log.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
		}
	case "eth_getBlockByHash":
		block := eth1.BlockHeaderByHash(s.genesisTime)
		response := requestItem.response(block)
		if err := codec.Write(ctx, response); err != nil {
			log.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
		}
	case "eth_getLogs":
		response := requestItem.response(s.eth1Logs[:s.numDepositsReadyToSend])
		if err := codec.Write(ctx, response); err != nil {
			log.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
		}
	case "eth_call":
		if strings.Contains(stringRep, eth1.DepositMethodID()) {
			count := eth1.DepositCount(s.deposits[:s.numDepositsReadyToSend])
			depCount, err := eth1.PackDepositCount(count[:])
			if err != nil {
				log.WithError(err).Error("Could not respond to HTTP request")
				w.WriteHeader(http.StatusInternalServerError)
			}
			response := requestItem.response(fmt.Sprintf("%#x", depCount))
			if err := codec.Write(ctx, response); err != nil {
				log.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}
		if strings.Contains(stringRep, eth1.DepositLogsID()) {
			root, err := eth1.DepositRoot(s.deposits[:s.numDepositsReadyToSend])
			if err != nil {
				log.WithError(err).Error("Could not respond to HTTP request")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			response := requestItem.response(fmt.Sprintf("%#x", root))
			if err := codec.Write(ctx, response); err != nil {
				log.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}
		s.defaultResponse(w)
	default:
		s.defaultResponse(w)
	}
}

func (s *server) defaultResponse(w http.ResponseWriter) {
	log.Error("Could not respond to HTTP request")
	w.WriteHeader(http.StatusBadRequest)
}

func (s *server) ServeWebsocket() http.Handler {
	return websocket.Server{
		Handler: func(conn *websocket.Conn) {
			codec := newWebsocketCodec(conn)
			wsHandler := &websocketHandler{
				blockNum:      0,
				close:         make(chan bool),
				readOperation: make(chan []*jsonrpcMessage),
				readErr:       make(chan error),
			}

			defer codec.Close()
			// Listen to read events from the codec and dispatch events or errors accordingly.
			go wsHandler.websocketReadLoop(codec)
			go wsHandler.dispatchWebsocketEventLoop(codec)
			<-codec.Closed()
		},
	}
}

func (w *websocketHandler) dispatchWebsocketEventLoop(codec ServerCodec) {
	tick := time.NewTicker(time.Second * 10)
	var latestSubID rpc.ID
	for {
		select {
		case <-w.close:
			return
		case err := <-w.readErr:
			log.WithError(err).Error("Could not read data from request")
			return
		case <-tick.C:
			head := eth1.LatestChainHead(w.blockNum)
			data, _ := json.Marshal(head)
			params, _ := json.Marshal(&subscriptionResult{ID: string(latestSubID), Result: data})
			ctx := context.Background()
			item := &jsonrpcMessage{
				Version: "2.0",
				Method:  "eth_subscription",
				Params:  params,
			}
			if err := codec.Write(ctx, item); err != nil {
				log.Error(err)
				continue
			}
			w.blockNum++
		case msgs := <-w.readOperation:
			sub := &rpc.Subscription{ID: rpc.NewID()}
			item := &jsonrpcMessage{
				Version: msgs[0].Version,
				ID:      msgs[0].ID,
			}
			latestSubID = sub.ID
			newItem := item.response(sub)
			if err := codec.Write(context.Background(), newItem); err != nil {
				log.Error(err)
				continue
			}
		}
	}
}

func (w *websocketHandler) websocketReadLoop(codec ServerCodec) {
	for {
		select {
		case <-w.close:
			return
		default:
			msgs, _, err := codec.Read()
			if _, ok := err.(*json.SyntaxError); ok {
				if err := codec.Write(context.Background(), errorMessage(err)); err != nil {
					log.Error(err)
					continue
				}
			}
			if err != nil {
				w.readErr <- err
				return
			}
			w.readOperation <- msgs
		}
	}
}

func (s *server) listenForDepositTrigger() {
	reader := bufio.NewReader(os.Stdin)
	for {
		maxAllowed := len(s.deposits) - s.numDepositsReadyToSend
		log.Printf(
			"Enter the number of new eth2 deposits to trigger (max allowed %d): ",
			maxAllowed,
		)
		fmt.Print(">> ")
		line, _, err := reader.ReadLine()
		if err != nil {
			log.Error(err)
			continue
		}
		num, err := strconv.Atoi(string(line))
		if err != nil {
			log.Error(err)
		}
		if num > maxAllowed {
			log.Errorf(
				"You have already sent %d/%d available deposits in keystore, cannot submit more",
				len(s.deposits),
				s.numDepositsReadyToSend,
			)
			continue
		}
		s.numDepositsReadyToSend += num
	}
}
