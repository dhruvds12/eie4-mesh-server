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
	fsClient   *firestore.Client
	authClient *auth.Client
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

type UserSyncReq struct {
	UserID string    `json:"userId"`
	Since  int64     `json:"since"`
	Uplink []Message `json:"uplink"`
}

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
// for user‐driven signup
func registerUser(w http.ResponseWriter, r *http.Request) {
	// uid := bearerUID(r); if uid == "" { http.Error(w,"unauth",401); return }
	var req RegisterReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	doc := fsClient.Collection(collectionUsers).Doc(req.UserID)
	if _, err := doc.Get(r.Context()); err == nil {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(req)
		return
	}
	_, err := doc.Set(r.Context(), map[string]any{
		"phoneDigits": req.PhoneDigits,
		"displayName": req.DisplayName,
		"createdAt":   time.Now(),
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(req)
}

// for mesh‐node signup (no auth, just trust the gwId you send)
type NodeRegisterReq struct {
	GwID string `json:"gwId"`
}

func registerNode(w http.ResponseWriter, r *http.Request) {
	// uid := bearerUID(r); if uid == "" { http.Error(w,"unauth",401); return }
	var req NodeRegisterReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	doc := fsClient.Collection(collectionGW).Doc(req.GwID)
	if _, err := doc.Get(r.Context()); err == nil {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(req)
		return
	}
	_, err := doc.Set(r.Context(), map[string]any{
		"createdAt": time.Now(),
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(req)
}

func message(w http.ResponseWriter, r *http.Request) {
	// uid := bearerUID(r); if uid == "" { http.Error(w,"unauth",401); return }
	var m Message
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	if _, err := fsClient.Collection(collectionUsers).Doc(m.Src).Get(r.Context()); err != nil {
		http.Error(w, "unknown user", http.StatusUnauthorized)
		return
	}

	if m.MsgID == "" {
		m.MsgID = randID()
	}
	m.TS = time.Now().Unix()
	_, err := fsClient.Collection(collectionMsgs).Doc(m.MsgID).
		Set(r.Context(), map[string]any{
			"srcID": m.Src, "dstID": m.Dst,
			"payload": m.Body, "tsCreated": time.Now(),
			"status": "new",
		})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(202)
}

// ---------- syncNode: for gateways ----------
func syncNode(w http.ResponseWriter, r *http.Request) {
	var req SyncReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	//  ensure this gateway is registered
	if _, err := fsClient.Collection(collectionGW).Doc(req.GwID).Get(r.Context()); err != nil {
		http.Error(w, "unknown gateway", http.StatusUnauthorized)
		return
	}

	//  store presence for all seen users
	for _, uid := range req.Seen {
		fsClient.Collection(collectionPres).Doc(uid).Set(r.Context(), map[string]any{
			"gwId":     req.GwID,
			"lastSeen": time.Now(),
		})
	}

	//  store uplink messages, build ack list
	ack := make([]string, 0, len(req.Uplink))
	for _, m := range req.Uplink {
		if m.MsgID == "" {
			m.MsgID = randID()
		}
		ack = append(ack, m.MsgID)
		_, _ = fsClient.Collection(collectionMsgs).Doc(m.MsgID).Set(r.Context(), map[string]any{
			"srcID":        m.Src,
			"dstID":        m.Dst,
			"payload":      m.Body,
			"tsCreated":    time.Now(),
			"deliveredVia": req.GwID,
			"status":       "new",
		})
	}

	//  collect any new messages for this gateway’s users
	down := []Message{}
	iter := fsClient.Collection(collectionMsgs).
		Where("status", "==", "new").
		Documents(r.Context())
	for {
		doc, err := iter.Next()
		if err != nil {
			break
		}
		dst := doc.Data()["dstID"].(string)

		// only deliver if this gateway “sees” that user
		pres, err := fsClient.Collection(collectionPres).Doc(dst).Get(r.Context())
		if err != nil || pres.Data()["gwId"] != req.GwID {
			continue
		}

		// append to down and mark routed
		down = append(down, Message{
			MsgID: doc.Ref.ID,
			Src:   doc.Data()["srcID"].(string),
			Dst:   dst,
			Body:  doc.Data()["payload"].(string),
			TS:    doc.Data()["tsCreated"].(time.Time).Unix(),
		})
		_, _ = doc.Ref.Update(r.Context(), []firestore.Update{
			{Path: "status", Value: "routed"},
			{Path: "tsDelivered", Value: time.Now()},
		})
	}

	// respond
	resp := SyncResp{
		Ack:   ack,
		Down:  down,
		Sleep: 15,
	}
	json.NewEncoder(w).Encode(resp)
}

// ---------- syncUser: for app‐to‐server user messaging ----------
func syncUser(w http.ResponseWriter, r *http.Request) {
	var req UserSyncReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fsClient.Collection(collectionPres).Doc(req.UserID).Delete(r.Context())

	// ensure this user is registered
	if _, err := fsClient.Collection(collectionUsers).Doc(req.UserID).Get(r.Context()); err != nil {
		http.Error(w, "unknown user", http.StatusUnauthorized)
		return
	}

	// store any uplink messages from the user
	ack := make([]string, 0, len(req.Uplink))
	for _, m := range req.Uplink {
		if m.MsgID == "" {
			m.MsgID = randID()
		}
		ack = append(ack, m.MsgID)
		_, _ = fsClient.Collection(collectionMsgs).Doc(m.MsgID).Set(r.Context(), map[string]any{
			"srcID":     m.Src,
			"dstID":     m.Dst,
			"payload":   m.Body,
			"tsCreated": time.Now(),
			"status":    "new",
		})
	}

	//  fetch downlink messages for this user only
	down := []Message{}
	query := fsClient.Collection(collectionMsgs).
		Where("dstID", "==", req.UserID).
		Where("status", "==", "new")
	// if you want to respect the Since timestamp:
	if req.Since > 0 {
		cutoff := time.Unix(req.Since, 0)
		query = query.Where("tsCreated", ">=", cutoff)
	}
	iter := query.Documents(r.Context())
	for {
		doc, err := iter.Next()
		if err != nil {
			break
		}
		data := doc.Data()
		down = append(down, Message{
			MsgID: doc.Ref.ID,
			Src:   data["srcID"].(string),
			Dst:   data["dstID"].(string),
			Body:  data["payload"].(string),
			TS:    data["tsCreated"].(time.Time).Unix(),
		})
		_, _ = doc.Ref.Update(r.Context(), []firestore.Update{
			{Path: "status", Value: "routed"},
			{Path: "tsDelivered", Value: time.Now()},
		})
	}

	// respond (you can drop Sleep or set to zero)
	resp := SyncResp{
		Ack:   ack,
		Down:  down,
		Sleep: 0,
	}
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

	// // create the Auth client
	// authClient, err = app.Auth(ctx)
	// if err != nil {
	// 	log.Fatalf("error getting Auth client: %v", err)
	// }

	// create the Firestore client
	fsClient, err = app.Firestore(ctx)
	if err != nil {
		log.Fatalf("error initializing Firestore client: %v", err)
	}
	defer fsClient.Close()

	// set up your router
	r := chi.NewRouter()
	r.Use(middleware.Logger)

	r.Post("/v1/register/user", registerUser)
	r.Post("/v1/register/node", registerNode)
	r.Post("/v1/message", message) // you can leave this open or split too
	r.Post("/v1/sync/node", syncNode)
	r.Post("/v1/sync/user", syncUser)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	addr := ":8443"
	log.Println("serving", addr)
	log.Fatal(http.ListenAndServe(addr, r))
}
