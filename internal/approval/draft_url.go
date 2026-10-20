package approval

import "strings"

// DraftPageURL is where a requester opens an OA draft to submit it: the OA's
// public base URL and /tasks/<draft id>. The demo OA builds its draft links
// with it, so the Gateway can rebuild a link from the stored draft id.
func DraftPageURL(publicBaseURL, draftID string) string {
	return strings.TrimRight(publicBaseURL, "/") + "/tasks/" + draftID
}
