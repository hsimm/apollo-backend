package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/sideshow/apns2"
	"github.com/sideshow/apns2/payload"
	"github.com/sideshow/apns2/token"

	"github.com/christianselig/apollo-backend/internal/data"
	"github.com/christianselig/apollo-backend/internal/reddit"
)

type application struct {
	logger *log.Logger
	db     *sql.DB
	models *data.Models
	client *reddit.Client
}

var workers int = runtime.NumCPU() * 8

func accountWorker(id int, rc *reddit.Client, db *sql.DB, logger *log.Logger, quit chan bool) {
	authKey, err := token.AuthKeyFromBytes([]byte(os.Getenv("APPLE_KEY_PKEY")))
	token := &token.Token{
		AuthKey: authKey,
		KeyID:   os.Getenv("APPLE_KEY_ID"),
		TeamID:  os.Getenv("APPLE_TEAM_ID"),
	}

	if err != nil {
		log.Fatal("token error:", err)
	}

	client := apns2.NewTokenClient(token)

	for {
		select {
		case <-quit:
			return
		default:
			now := time.Now().UTC().Unix()

			tx, err := db.Begin()

			if err != nil {
				log.Fatal(err)
				continue
			}

			query := `
				SELECT id, access_token, refresh_token, expires_at, last_message_id FROM accounts
				WHERE last_checked_at <= $1 - 5
				ORDER BY last_checked_at
				LIMIT 1
				FOR UPDATE SKIP LOCKED`
			args := []interface{}{now}

			account := &data.Account{}
			err = tx.QueryRow(query, args...).Scan(&account.ID, &account.AccessToken, &account.RefreshToken, &account.ExpiresAt, &account.LastMessageID)

			if account.ID == 0 {
				time.Sleep(100 * time.Millisecond)
				tx.Commit()
				continue
			}

			logger.Printf("Worker #%d, account %d", id, account.ID)

			_, err = tx.Exec(`UPDATE accounts SET last_checked_at = $1 WHERE id = $2`, now, account.ID)

			rac := rc.NewAuthenticatedClient(account.RefreshToken, account.AccessToken)
			if account.ExpiresAt < now {
				tokens, _ := rac.RefreshTokens()
				tx.Exec(`UPDATE accounts SET access_token = $1, refresh_token = $2, expires_at = $3 WHERE id = $4`,
					tokens.AccessToken, tokens.RefreshToken, now+3500, account.ID)
			}

			msgs, err := rac.MessageInbox(account.LastMessageID)
			if err != nil {
				log.Fatal(err)
			}

			if len(msgs.MessageListing.Messages) == 0 {
				tx.Commit()
				continue
			}

			// Set latest message we alerted on
			latestMsg := msgs.MessageListing.Messages[0]

			_, err = tx.Exec(`UPDATE accounts SET last_message_id = $1 WHERE id = $2`, latestMsg.FullName(), account.ID)
			if err != nil {
				log.Fatal(err)
			}

			devices := []string{}
			query = `
				SELECT apns_token FROM devices
				LEFT JOIN devices_accounts ON devices.id = devices_accounts.device_id
				WHERE devices_accounts.account_id = $1`

			rows, err := tx.Query(query, account.ID)
			if err != nil {
				logger.Fatal(err)
			}
			for rows.Next() {
				var device string
				rows.Scan(&device)
				devices = append(devices, device)
			}
			rows.Close()

			for _, msg := range msgs.MessageListing.Messages {
				for _, device := range devices {
					notification := &apns2.Notification{}
					notification.DeviceToken = device
					notification.Topic = "com.christianselig.Apollo"
					notification.Payload = payload.NewPayload().AlertTitle(msg.Subject).AlertBody(msg.Body)
					res, err := client.Push(notification)
					if err != nil {
						logger.Printf("Error sending push: %s", err)
					} else {
						logger.Printf("Push response: %v %v %v\n", res.StatusCode, res.ApnsID, res.Reason)
					}
				}
			}

			tx.Commit()
		}
	}
}

func main() {
	logger := log.New(os.Stdout, "", log.Ldate|log.Ltime)

	if err := godotenv.Load(); err != nil {
		logger.Printf("Couldn't find .env so I will read from existing ENV.")
	}

	rc := reddit.NewClient(os.Getenv("REDDIT_CLIENT_ID"), os.Getenv("REDDIT_CLIENT_SECRET"))

	dburl, ok := os.LookupEnv("DATABASE_CONNECTION_POOL_URL")
	if !ok {
		dburl = os.Getenv("DATABASE_URL")
	}

	db, err := sql.Open("postgres", fmt.Sprintf("%s?binary_parameters=yes", dburl))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	logger.Printf("Starting with %d workers.", workers)

	db.SetMaxOpenConns(workers)

	// This is a very conservative value -- seen as most of the work that is done in these jobs is
	//
	runtime.GOMAXPROCS(workers)
	quitCh := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		go accountWorker(i, rc, db, logger, quitCh)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	<-sigs

	for i := 0; i < workers; i++ {
		quitCh <- true
	}
	os.Exit(0)
}
