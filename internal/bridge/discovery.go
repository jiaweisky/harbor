package bridge

import (
	"encoding/json"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// NewCardsHandler serves every agent card as a JSON array. This is an
// additive, non-standard endpoint used by CodeBuddy's remote-agents discovery,
// which expects an array (or {"cards":[...]}) rather than a single card.
func NewCardsHandler(cards []*a2a.AgentCard) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cards)
	})
}
