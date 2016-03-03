package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/context"
	"github.com/gorilla/websocket"
	"github.com/satori/go.uuid"
)

var (
	cfg         Config
	logger      *log.Logger
	proxyserver *ProxyServer
	err         error
	host        string
	port        string
)

type EncryptorSecret struct {
	Key string `json:"base64EncodedKeyBytes"`
	Iv  string `json:"base64EncodedIvBytes"`
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func ProxyConnection(src, dest net.Conn) {

	buffer := make([]byte, 1024)

	for {
		n, err := src.Read(buffer)
		if err != nil {
			log.Fatalf("Unable to read from socket" + err.Error())
		}

		n, err = dest.Write(buffer[0:n])
		if err != nil {
			log.Fatalf("Unable to write to socket" + err.Error())
		}
	}
}

// Given a local session, establish a Websocket <-> HTTPS tunnel to the
// Xenserver
func handleVncWebsocketProxy(w http.ResponseWriter, r *http.Request) {

	wsConn, err := upgrader.Upgrade(w, r, nil)

	if err != nil {
		logger.Printf(err.Error())
		return
	}

	dataType, data, err := wsConn.ReadMessage()
	if err != nil {
		_ = wsConn.WriteMessage(dataType, []byte("Fail: "+err.Error()))
		return
	}

	tcpAddr, err := net.ResolveTCPAddr("tcp", string(data))
	if err != nil {
		errorMsg := "FAIL(net resolve tcp addr): " + err.Error()
		logger.Println(errorMsg)
		_ = wsConn.WriteMessage(websocket.CloseMessage, []byte(errorMsg))
		return
	}

	tcpConn, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		errorMsg := "FAIL(net dial tcp): " + err.Error()
		logger.Println(errorMsg)
		_ = wsConn.WriteMessage(websocket.CloseMessage, []byte(errorMsg))
		return
	}

	proxyserver = NewProxyServer(wsConn, tcpConn)
	go proxyserver.doProxy()
}

// Decypt and get the tunnel URL and xenserver session ID, setup a new local session
// with all the variables and serve the vnc.html.
func handleNewConsoleConnection(w http.ResponseWriter, r *http.Request) {
	var sessionId string
	logger.Printf("new connection from: %s", r.RemoteAddr)
	path := r.URL.Query().Get("path")

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if path == "" {
		log.Printf("Setting new session !!!")

		token := r.URL.Query().Get("token")
		consoleSession, err := NewConsoleSession(cfg.Server.EncryptionKey, cfg.Server.EncryptionIv, token)
		if err != nil {
			log.Printf("error creating console session " + err.Error())
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		sessionId = uuid.NewV4().String()
		SessionMap[sessionId] = consoleSession
		http.Redirect(w, r, "/static/vnc_auto.html?path="+sessionId, http.StatusFound)

	} else {
		log.Printf("got session %s ", path)
		consoleSession := SessionMap[path]

		if consoleSession == nil {
			log.Printf("Unable to find session for " + path)
			http.Error(w, "Not Found", http.StatusInternalServerError)
		}

		http.ServeFile(w, r, "static/vnc_auto.html")

	}

}

func handleStatic(w http.ResponseWriter, r *http.Request) {
	log.Printf("static get : %s", r.URL.Path[1:])
	http.ServeFile(w, r, r.URL.Path[1:])
}

//We get the encryption key from the console proxy
func handleSetEncryptorPassword(w http.ResponseWriter, r *http.Request) {

	//Only allow this from localhost
	parts := strings.Split(r.RemoteAddr, ":")
	if parts[0] != "127.0.0.1" {
		log.Printf("Request from non-local host")
		return
	}

	if secretJson := r.URL.Query().Get("secret"); secretJson != "" {
		var secret EncryptorSecret
		log.Println("Secret json: " + secretJson)
		err := json.Unmarshal([]byte(secretJson), &secret)
		if err != nil {
			log.Printf("Unable to decode the secret sent from the servelet")
			return
		}
		cfg.Server.SetEncryptionKey(secret.Key)
		cfg.Server.SetEncryptionIv(secret.Iv)
		fmt.Fprintf(w, "Password was set %s:%s ", secret.Key, secret.Iv)
	}
}

func initXenConnection(w http.ResponseWriter, r *http.Request) net.Conn {
	log.Printf("VNC request " + r.URL.String())

	paths := strings.Split(r.URL.Path, "/")

	if len(paths) < 3 || !IsUUID(paths[2]) || SessionMap[paths[2]] == nil {
		log.Printf("Unable to find session")
		http.Error(w, "Not Found", http.StatusInternalServerError)
	}

	session := SessionMap[paths[2]]
	log.Printf("session:\n%+v\n", session)

	if session.ClientTunnelSession == "" || session.ClientTunnelUrl == "" {
		log.Printf("Unable to find Tunnel URL or Tunnel Session")
		delete(SessionMap, paths[2])
		http.Error(w, "Not Found", http.StatusInternalServerError)
		return nil
	}

	//open session to Xenserver
	tunnelUrl, err := url.Parse(session.ClientTunnelUrl)
	if err != nil {
		log.Printf("Unable to parse session URL")
		delete(SessionMap, paths[2])
		http.Error(w, "Not Found", http.StatusInternalServerError)
		return nil
	}

	host := tunnelUrl.Host
	uri := tunnelUrl.RequestURI()

	data := fmt.Sprintf("CONNECT %s HTTP/1.0\r\nHost: %s\r\nCookie: session_id=%s\r\n\r\n",
		uri, host, session.ClientTunnelSession)

	xenConn, err := tls.Dial("tcp", host+":443", &tls.Config{InsecureSkipVerify: true})
	_, err = xenConn.Write([]byte(data))
	if err != nil {
		logger.Println(err.Error())
		http.Error(w, "Not Found", http.StatusInternalServerError)
		return nil
	}

	buffer := make([]byte, 1024)
	_, err = xenConn.Read(buffer)

	if err != nil {
		log.Printf("Error reading data from xenserver " + err.Error())
		http.Error(w, "Not Found", http.StatusInternalServerError)
		return nil
	}

	if !strings.Contains(string(buffer), "200 OK") {
		log.Printf("non 200 response from xenserver https")
		http.Error(w, "Not Found", http.StatusInternalServerError)
		return nil
	}

	return xenConn
}

func main() {

	logger.Printf("listening on %s\n", cfg.Server.Addr())
	http.HandleFunc("/console", handleNewConsoleConnection)
	http.HandleFunc("/setEncryptorPassword", handleSetEncryptorPassword)
	http.HandleFunc("/static/", handleStatic)
	http.HandleFunc("/vnc/", handleVncWebsocketProxy)
	http.ListenAndServe(cfg.Server.Addr(), context.ClearHandler(http.DefaultServeMux))
}