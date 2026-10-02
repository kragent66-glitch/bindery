package models

import "time"

// CalibreDeliveryState is where one ebook file stands in the Calibre delivery
// ledger (#2832). The values are the CHECK constraint on
// calibre_deliveries.state, so keep the two in step.
type CalibreDeliveryState string

const (
	// CalibreDeliveryPending is waiting for its next_attempt_at.
	CalibreDeliveryPending CalibreDeliveryState = "pending"
	// CalibreDeliveryDelivered reached Calibre; calibre_id holds the target's id.
	CalibreDeliveryDelivered CalibreDeliveryState = "delivered"
	// CalibreDeliveryFailed gave up after a terminal error. Only an explicit
	// retry puts it back in the queue.
	CalibreDeliveryFailed CalibreDeliveryState = "failed"
	// CalibreDeliverySkipped was deliberately not sent (the outcome says why).
	CalibreDeliverySkipped CalibreDeliveryState = "skipped"
)

// CalibreDelivery is one row of the calibre_deliveries ledger: one ebook
// book_files row and whether it has been sent to the Calibre push target.
//
// CalibreID is the record id in the push target library. It is deliberately
// not books.calibre_id, which keeps meaning the id in the library at
// calibre.library_path.
type CalibreDelivery struct {
	ID            int64                `json:"id"`
	BookID        int64                `json:"bookId"`
	BookFileID    int64                `json:"bookFileId"`
	EditionID     *int64               `json:"editionId,omitempty"`
	FilePath      string               `json:"filePath"`
	Format        string               `json:"format"`
	State         CalibreDeliveryState `json:"state"`
	Outcome       string               `json:"outcome"`
	Attempts      int                  `json:"attempts"`
	LastError     string               `json:"lastError"`
	LastErrorCode string               `json:"lastErrorCode"`
	CalibreID     *int64               `json:"calibreId,omitempty"`
	TargetLibrary string               `json:"targetLibrary"`
	NextAttemptAt time.Time            `json:"nextAttemptAt"`
	CreatedAt     time.Time            `json:"createdAt"`
	UpdatedAt     time.Time            `json:"updatedAt"`
	DeliveredAt   *time.Time           `json:"deliveredAt,omitempty"`
}

// CalibreDeliverySummary is the ledger at a glance: row counts per state and
// the most recent delivery. LastDeliveredAt is nil when nothing was delivered.
type CalibreDeliverySummary struct {
	Pending         int        `json:"pending"`
	Delivered       int        `json:"delivered"`
	Failed          int        `json:"failed"`
	Skipped         int        `json:"skipped"`
	LastDeliveredAt *time.Time `json:"lastDeliveredAt,omitempty"`
}

// CalibreDeliveryListItem is a ledger row with the names the settings queue
// view shows next to it.
type CalibreDeliveryListItem struct {
	CalibreDelivery
	BookTitle  string `json:"bookTitle"`
	AuthorName string `json:"authorName"`
}
