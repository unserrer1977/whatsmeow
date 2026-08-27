package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"
	"github.com/skip2/go-qrcode"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var (
	client   *whatsmeow.Client
	apiKey   string
	qrValue  string
	qrMutex  sync.RWMutex
)

func setQR(value string) {
	qrMutex.Lock()
	qrValue = value
	qrMutex.Unlock()
}

func getQR() string {
	qrMutex.RLock()
	defer qrMutex.RUnlock()
	return qrValue
}

func authorised(r *http.Request) bool {
	key := r.Header.Get("X-API-Key")

	// Makes opening the QR page in a browser easier.
	if key == "" {
		key = r.URL.Query().Get("key")
	}

	return key != "" && key == apiKey
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"running":   true,
		"connected": client != nil && client.IsConnected(),
		"logged_in": client != nil && client.IsLoggedIn(),
	})
}

func qrHandler(w http.ResponseWriter, r *http.Request) {
	if !authorised(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if client.IsLoggedIn() {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `
			<h1>WhatsApp is connected</h1>
			<p>You have successfully linked this Railway service to WhatsApp.</p>
		`)
		return
	}

	value := getQR()

	if value == "" {
		http.Error(
			w,
			"QR code is not ready. Refresh this page in a few seconds.",
			http.StatusServiceUnavailable,
		)
		return
	}

	png, err := qrcode.Encode(value, qrcode.Medium, 512)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}

func groupsHandler(w http.ResponseWriter, r *http.Request) {
	if !authorised(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if !client.IsLoggedIn() {
		http.Error(w, "WhatsApp is not connected", http.StatusServiceUnavailable)
		return
	}

	groups, err := client.GetJoinedGroups(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	result := make([]map[string]string, 0, len(groups))

	for _, group := range groups {
		result = append(result, map[string]string{
			"name": group.Name,
			"jid":  group.JID.String(),
		})
	}

	writeJSON(w, http.StatusOK, result)
}

type sendRequest struct {
	To      string `json:"to"`
	Message string `json:"message"`
}

func sendHandler(w http.ResponseWriter, r *http.Request) {
	if !authorised(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Use POST", http.StatusMethodNotAllowed)
		return
	}

	if !client.IsLoggedIn() {
		http.Error(w, "WhatsApp is not connected", http.StatusServiceUnavailable)
		return
	}

	var req sendRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.To == "" || req.Message == "" {
		http.Error(w, "to and message are required", http.StatusBadRequest)
		return
	}

	var target types.JID
	var err error

	// Group IDs end in @g.us
	if strings.HasSuffix(req.To, "@g.us") {
		target, err = types.ParseJID(req.To)

		if err != nil {
			http.Error(w, "Invalid group ID", http.StatusBadRequest)
			return
		}
	} else {
		// Normal phone number, e.g. +447700900123
		results, checkErr := client.IsOnWhatsApp(
			r.Context(),
			[]string{req.To},
		)

		if checkErr != nil {
			http.Error(w, checkErr.Error(), http.StatusInternalServerError)
			return
		}

		if len(results) == 0 || !results[0].IsIn {
			http.Error(
				w,
				"That number does not appear to be on WhatsApp",
				http.StatusBadRequest,
			)
			return
		}

		target = results[0].JID
	}

	response, err := client.SendMessage(
		r.Context(),
		target,
		&waE2E.Message{
			Conversation: proto.String(req.Message),
		},
	)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":     "sent",
		"message_id": response.ID,
		"to":         target.String(),
	})
}

func main() {
	apiKey = os.Getenv("API_KEY")

	if apiKey == "" {
		log.Fatal("API_KEY environment variable is required")
	}

	dataDir := os.Getenv("DATA_DIR")

	if dataDir == "" {
		dataDir = "/data"
	}

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		log.Fatal(err)
	}

	dbPath := filepath.Join(dataDir, "whatsmeow.db")

	dbLog := waLog.Stdout("Database", "INFO", true)

	ctx := context.Background()

	container, err := sqlstore.New(
		ctx,
		"sqlite3",
		"file:"+dbPath+"?_foreign_keys=on",
		dbLog,
	)

	if err != nil {
		log.Fatal(err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		log.Fatal(err)
	}

	clientLog := waLog.Stdout("WhatsApp", "INFO", true)

	client = whatsmeow.NewClient(deviceStore, clientLog)

	// First ever login
	if client.Store.ID == nil {

		qrChannel, err := client.GetQRChannel(ctx)
		if err != nil {
			log.Fatal(err)
		}

		if err := client.Connect(); err != nil {
			log.Fatal(err)
		}

		go func() {
			for event := range qrChannel {

				switch event.Event {

				case "code":
					setQR(event.Code)
					log.Println("QR code available at /qr")

				case "success":
					setQR("")
					log.Println("WhatsApp successfully paired")

				default:
					log.Println("QR event:", event.Event)
				}
			}
		}()

	} else {

		if err := client.Connect(); err != nil {
			log.Fatal(err)
		}

		log.Println("Connected using saved WhatsApp session")
	}

	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/qr", qrHandler)
	http.HandleFunc("/groups", groupsHandler)
	http.HandleFunc("/send", sendHandler)

	port := os.Getenv("PORT")

	if port == "" {
		port = "8080"
	}

	log.Println("HTTP server listening on port", port)

	log.Fatal(
		http.ListenAndServe("0.0.0.0:"+port, nil),
	)
}
