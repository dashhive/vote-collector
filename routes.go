package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/btcsuite/btcutil/base58"
	"github.com/dashhive/dashmsg"
	jwt "github.com/dgrijalva/jwt-go"
)

// JWTSecretKey is used to verify the JWT was signed w/the same, used for
// authorization.
// See also: https://jwt.io/#debugger
var JWTSecretKey []byte

// DashNetwork is used for validating the address network byte
var DashNetwork string

// ServeHTTP passes requests thru to the router.
func (s server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// routes defines the routes the server will handle
func (s *server) routes() {
	// health check
	s.router.HandleFunc("/api/health", s.handleHealthCheck())

	// route to record incoming votes
	s.router.HandleFunc("/api/vote", s.handleVote())

	s.router.HandleFunc("/api/candidates", s.handleCandidates())
	s.router.HandleFunc("/api/votingaddresses", s.handleVotingAddresses())
	s.router.HandleFunc("/api/mnlist", s.handleMNList())

	// audit routes
	// the public can view all votes once the voting has concluded
	s.router.HandleFunc("/api/validVotes", s.isAuthorizedOrTimely(s.handleValidVotes()))
	s.router.HandleFunc("/api/votes", s.isAuthorizedOrTimely(s.handleValidVotes()))

	s.router.HandleFunc("/api/allVotes", s.isAuthorizedOrTimely(s.handleAllVotes()))
	s.router.HandleFunc("/api/all-votes", s.isAuthorizedOrTimely(s.handleAllVotes()))

	// TODO: catch-all (404)
	s.router.PathPrefix("/").Handler(s.handleIndex())
}

// isAuthorizedOrTimely is used to wrap handlers that need authz during the vote
func (s *server) isAuthorizedOrTimely(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if time.Until(s.votingEnd) <= 0 {
			// votes are public once voting has ended
			f(w, r)
			return
		}

		bearerToken, ok := r.Header["Authorization"]
		if !ok {
			writeError(http.StatusUnauthorized, w, r)
			return
		}

		// strip the "Bearer " from the beginning
		actualTokenStr := strings.TrimPrefix(bearerToken[0], "Bearer ")

		// Parse and validate token from request header
		token, err := jwt.Parse(actualTokenStr, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return "invalid signing method", nil
			}
			return JWTSecretKey, nil
		})
		if err != nil {
			writeError(http.StatusUnauthorized, w, r)
			return
		}

		// JWT is valid, pass the request thru to protected route
		if token.Valid {
			f(w, r)
		}
	}
}

func (s *server) updateLists() error {
	s.candidatesUpdateMux.Lock()
	defer s.candidatesUpdateMux.Unlock()

	s.candidatesMux.RLock()
	stale := time.Since(s.candidatesUpdatedAt) > 15*time.Minute
	s.candidatesMux.RUnlock()

	if !stale {
		return nil
	}

	candidates, err := GSheetToCandidates(s.gsheetKey)
	if err != nil {
		// TODO: we want a way to signal this, yeah?
		return err
	}

	now := time.Now()

	var mnList map[string]MNInfo

	s.candidatesMux.RLock()
	// TODO handle error
	if s.candidatesUpdatedAt.Sub(s.votingEnd) > 0 && len(s.mnList) > 0 {
		mnList = s.mnList
	} else {
		if s.candidatesUpdatedAt.Sub(s.votingEnd) > 0 {
			fmt.Fprintf(os.Stderr, "BUG: Updating mnlist AFTER vote has closed (TODO fix)")
		}
		mnList = s.getMNList()
	}
	s.candidatesMux.RUnlock()

	// TODO keep track of voting key weight map[string][]string
	// (i.e. map[votingaddr][]collateraladdr)
	votingAddresses := []string{}
	for _, v := range mnList {
		votingAddresses = append(votingAddresses, v.VotingAddress)
	}

	s.candidatesMux.Lock()
	s.mnList = mnList
	s.votingAddresses = votingAddresses
	s.candidatesUpdatedAt = now
	s.candidates = candidates
	s.candidatesMux.Unlock()
	return nil
}

func (s *server) getMNList() map[string]MNInfo {
	c := &http.Client{
		Transport: &http.Transport{
			Dial: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).Dial,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}

	ch := make(chan struct{})
	//timer :=
	time.AfterFunc(5*time.Second, func() {
		close(ch)
	})
	// timer.Reset

	req, err := http.NewRequest("GET", s.mnlistURL, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return nil
	}
	req.Cancel = ch

	log.Println("Sending request...")
	resp, err := c.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return nil
	}
	defer resp.Body.Close()

	mninfo := map[string]MNInfo{}

	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&mninfo); nil != err {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return nil
	}

	return mninfo
}

// handleCandidates handles the candidates route
func (s *server) handleCandidates() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var ourCandidates []Candidate

		s.candidatesMux.RLock()
		if time.Since(s.votingStart) <= 0 {
			ourCandidates = []Candidate{}
		} else {
			ourCandidates = s.candidates
			//ourCandidates = make([]Candidate, len(s.candidates))
			//_ = copy(ourCandidates, s.candidates)
		}
		s.candidatesMux.RUnlock()
		go func() {
			err := s.updateLists()
			if nil != err {
				log.Printf("Failed to update candidate list: %v\n", err)
			}
		}()

		// Return response
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(ourCandidates)
	}
}

// handleVotingAddresses handles the voting keys route
func (s *server) handleVotingAddresses() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var ourAddrs []string

		s.candidatesMux.RLock()
		ourAddrs = s.votingAddresses
		s.candidatesMux.RUnlock()

		// Return response
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(ourAddrs)
	}
}

// handleMNList handles the full mnlist
func (s *server) handleMNList() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var ourMNList map[string]MNInfo

		s.candidatesMux.RLock()
		ourMNList = s.mnList
		s.candidatesMux.RUnlock()

		// Return response
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(ourMNList)

	}
}

// handleVote handles the vote route
func (s *server) handleVote() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if time.Since(s.votingStart) <= 0 {
			writeErrorMessage("E_VOTING_NOT_STARTED", http.StatusForbidden, w, r)
			return
		}
		if time.Until(s.votingEnd) <= 0 {
			writeErrorMessage("E_VOTING_CLOSED", http.StatusForbidden, w, r)
			return
		}

		// Parse vote body
		var v Vote
		err := json.NewDecoder(r.Body).Decode(&v)
		if err != nil {
			writeError(http.StatusBadRequest, w, r)
			return
		}
		v.CreatedAt = time.Now().UTC()

		// Very basic input validation. In the future the ideal solution would
		// be to validate signature as well.
		if !isValidAddress(v.Address, os.Getenv("DASH_NETWORK")) {
			writeErrorMessage("INVALID_NETWORK", http.StatusBadRequest, w, r)
			return
		}
		if err := dashmsg.MagicVerify(v.Address, []byte(v.Message), v.Signature); nil != err {
			writeErrorMessage("INVALID_SIGNATURE: "+err.Error(), http.StatusBadRequest, w, r)
		}

		// Insert vote
		err = s.db.Insert(&v)
		if err != nil {
			writeErrorMessage("E_DATABASE_WRITE", http.StatusInternalServerError, w, r)
			return
		}

		// Return response
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(JSONResult{
			Status:  http.StatusCreated,
			Message: "Vote Recorded",
		})
	}
}

// handleValidVotes is the route for vote tallying, and returns only most
// current vote per MN collateral address.
// TODO: consider pagination if this gets too big.
func (s *server) handleValidVotes() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		votes, err := getCurrentVotesOnly(s.db)
		if err != nil {
			writeErrorMessage("E_DATABASE_GET_VALID", http.StatusInternalServerError, w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		err = enc.Encode(&votes)
		if err != nil {
			// this error can't actually happen (unless the client is already errors out)
			writeErrorMessage("E_RESPONSE_VALID_VOTES", http.StatusInternalServerError, w, r)
			return
		}
	}
}

// handleAllVotes is the route for listing all votes, including old, superceded
// ones. Use with caution! (For audit purposes only.)
// TODO: consider pagination if this gets too big.
func (s *server) handleAllVotes() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		votes, err := getAllVotes(s.db)
		if err != nil {
			writeErrorMessage("E_DATABASE_GET_ALL", http.StatusInternalServerError, w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		err = enc.Encode(&votes)
		if err != nil {
			// this error can't actually happen (unless the client is already errors out)
			writeErrorMessage("E_RESPONSE_ALL_VOTES", http.StatusInternalServerError, w, r)
			return
		}
	}
}

// handleHealthCheck handles the health check route, an unauthenticated route
// needed for load balancers to know this service is still "healthy".
func (s *server) handleHealthCheck() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(JSONResult{
			Status:  http.StatusOK,
			Message: http.StatusText(http.StatusOK),
		})
	}
}

// JSONErrorMessage represents the JSON structure of an error message to be
// returned.
type JSONErrorMessage struct {
	Message string `json:"message,omitempty"`
	Status  int    `json:"status"`
	URL     string `json:"url"`
	Error   string `json:"error"`
}

// JSONResult represents the JSON structure of the success message to be
// returned.
type JSONResult struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

// writeErrorMessage returns a JSON error with a helpful message.
func writeErrorMessage(msg string, errorCode int, w http.ResponseWriter, r *http.Request) {
	result := JSONErrorMessage{
		Message: msg,
		Status:  errorCode,
		URL:     r.URL.Path,
		Error:   http.StatusText(errorCode),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(errorCode)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(result)
}

// writeError returns a generic JSON error blob.
func writeError(errorCode int, w http.ResponseWriter, r *http.Request) {
	msg := JSONErrorMessage{
		Status: errorCode,
		URL:    r.URL.Path,
		Error:  http.StatusText(errorCode),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(errorCode)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(msg)
}

// handleIndex is catch-all route handler.
func (s *server) handleIndex() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(http.StatusNotFound, w, r)
	}
}

func init() {
	JWTSecretKey = []byte(os.Getenv("JWT_SECRET_KEY"))
}

// helper methods

// isValidAddress checks if a given string is a valid Dash address
func isValidAddress(addr string, dashNetwork string) bool {
	decoded, version, err := base58.CheckDecode(addr)
	if err != nil {
		return false
	}

	switch dashNetwork {
	case "mainnet":
		if version != 0x4c && version != 0x10 {
			return false
		}
	case "testnet":
		if version != 0x8c && version != 0x13 {
			return false
		}
	default: // only mainnet and testnet supported for now
		return false
	}

	return len(decoded) == 20
}
