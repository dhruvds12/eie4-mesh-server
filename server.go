package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

    "cloud.google.com/go/firestore"
    firebase "firebase.google.com/go/v4"
    "firebase.google.com/go/v4/auth"
    "github.com/go-chi/chi/v5"
    "github.com/go-chi/chi/v5/middleware"
    "google.golang.org/api/option"
)

const (
	projectID       = "eie4-mesh-network"
	collectionUsers = "users"
	collectionMsgs  = "messages"
	collectionGW    = "gateways"
	collectionPres  = "presence"
)

var (
	fsClient *firestore.Client
	authClient   *auth.Client
)

// ---------- models ----------
type (
	RegisterReq struct {
		UserID      string `json:"userID"`
		PhoneDigits string `json:"phoneDigits"`
		DisplayName string `json:"displayName,omitempty"`
	}
	Message struct {
		MsgID string `json:"msgId"`
		Src   string `json:"src"`
		Dst   string `json:"dst"`
		Body  string `json:"body"` // base64
		TS    int64  `json:"ts"`
	}
	SyncReq struct {
		GwID   string    `json:"gwId"`
		Since  int64     `json:"since"`
		Seen   []string  `json:"seen"`
		Uplink []Message `json:"uplink"`
	}
	SyncResp struct {
		Ack   []string  `json:"ack"`
		Down  []Message `json:"down"`
		Sleep int       `json:"sleep"`
	}
)

// ---------- helpers ----------
func bearerUID(r *http.Request) string {
    header := r.Header.Get("Authorization")
    if !strings.HasPrefix(header, "Bearer ") {
        return ""
    }
    idTok := strings.TrimPrefix(header, "Bearer ")
    tok, err := authClient.VerifyIDToken(r.Context(), idTok)
    if err != nil {
        return ""
    }
    return tok.UID
}

func randID() string {
	b := make([]byte, 9)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------- handlers ----------
func register(w http.ResponseWriter, r *http.Request) {
	uid := bearerUID(r)
	if uid == "" {
		http.Error(w, "unauth", 401); return
	}
	var req RegisterReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400); return
	}
	doc := fsClient.Collection(collectionUsers).Doc(req.UserID)
	_, err := doc.Get(r.Context())
	if err == nil {
		w.WriteHeader(200); json.NewEncoder(w).Encode(req); return
	}
	_, err = doc.Set(r.Context(), map[string]any{
		"anonUid":     uid,
		"phoneDigits": req.PhoneDigits,
		"displayName": req.DisplayName,
		"createdAt":   time.Now(),
	})
	if err != nil { http.Error(w, err.Error(), 500); return }
	json.NewEncoder(w).Encode(req)
}

func message(w http.ResponseWriter, r *http.Request) {
	uid := bearerUID(r); if uid == "" { http.Error(w,"unauth",401); return }
	var m Message
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, err.Error(), 400); return
	}
	if m.MsgID == "" { m.MsgID = randID() }
	m.TS = time.Now().Unix()
	_, err := fsClient.Collection(collectionMsgs).Doc(m.MsgID).
		Set(r.Context(), map[string]any{
			"srcID": m.Src, "dstID": m.Dst,
			"payload": m.Body, "tsCreated": time.Now(),
			"status": "new",
		})
	if err != nil { http.Error(w, err.Error(), 500); return }
	w.WriteHeader(202)
}

func sync(w http.ResponseWriter, r *http.Request) {
	uid := bearerUID(r); if uid == "" { http.Error(w,"unauth",401); return }
	var req SyncReq
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/cbor") {
		decoder := json.NewDecoder(r.Body) // fallback if you don’t wire CBOR yet
		if err := decoder.Decode(&req); err != nil { http.Error(w, err.Error(),400);return }
	} else if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400); return
	}

	// store presence
	for _, u := range req.Seen {
		fsClient.Collection(collectionPres).Doc(u).Set(r.Context(), map[string]any{
			"gwId": req.GwID, "lastSeen": time.Now(),
		})
	}

	// store uplink messages
	ack := make([]string, 0, len(req.Uplink))
	for _, m := range req.Uplink {
		if m.MsgID == "" { m.MsgID = randID() }
		ack = append(ack, m.MsgID)
		_, _ = fsClient.Collection(collectionMsgs).Doc(m.MsgID).Set(r.Context(), map[string]any{
			"srcID": m.Src, "dstID": m.Dst,
			"payload": m.Body, "tsCreated": time.Now(),
			"deliveredVia": req.GwID, "status": "new",
		})
	}

	// simple routing: pull messages newer than req.Since for this gateway’s users
	down := []Message{}
	iter := fsClient.Collection(collectionMsgs).
		Where("status", "==", "new").
		Documents(r.Context())
	for {
		doc, err := iter.Next()
		if err != nil { break }
		dst := doc.Data()["dstID"].(string)
		// send only if this GW recently saw dst
		pres, err := fsClient.Collection(collectionPres).Doc(dst).Get(r.Context())
		if err == nil && pres.Data()["gwId"] == req.GwID {
			down = append(down, Message{
				MsgID: doc.Ref.ID,
				Src: doc.Data()["srcID"].(string),
				Dst: dst,
				Body: doc.Data()["payload"].(string),
				TS: doc.Data()["tsCreated"].(time.Time).Unix(),
			})
			doc.Ref.Update(r.Context(), []firestore.Update{
				{Path: "status", Value: "routed"},
				{Path: "tsDelivered", Value: time.Now()},
			})
		}
	}

	resp := SyncResp{Ack: ack, Down: down, Sleep: 15}
	json.NewEncoder(w).Encode(resp)
}

// ---------- main ----------
func main() {
    ctx := context.Background()

    // initialize Firebase App with your service account
    app, err := firebase.NewApp(ctx, nil,
        option.WithCredentialsFile("./secrets/sa.json"))
    if err != nil {
        log.Fatalf("error initializing Firebase App: %v", err)
    }

    // create the Auth client
    authClient, err = app.Auth(ctx)
    if err != nil {
        log.Fatalf("error getting Auth client: %v", err)
    }

    // create the Firestore client
    fsClient, err = app.Firestore(ctx)
    if err != nil {
        log.Fatalf("error initializing Firestore client: %v", err)
    }
    defer fsClient.Close()

    // set up your router
    r := chi.NewRouter()
    r.Use(middleware.Logger)
    r.Post("/v1/register", register)
    r.Post("/v1/message", message)
    r.Post("/v1/sync", sync)
    r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
        w.Write([]byte("ok"))
    })

    addr := ":8443"
    log.Println("serving", addr)
    log.Fatal(http.ListenAndServe(addr, r))
}
