package imagegen

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// ModelInfo describes one model endpoint offered by a provider.
type ModelInfo struct {
	ID          string
	Name        string
	Category    string // e.g. "text-to-image", "image-to-video"
	Description string
}

// falCatalogURL is fal's platform API, which is separate from the queue
// host that runs models.
const falCatalogURL = "https://api.fal.ai/v1"

// maxModelPages bounds pagination when listing models.
const maxModelPages = 200

// ListModels asks the provider which image and video models it offers.
func ListModels(ctx context.Context, o Options) ([]ModelInfo, error) {
	switch o.Type {
	case TypeFal:
		return newFal(o).listModels(ctx, o.CatalogURL)
	}
	return nil, fmt.Errorf("imagegen: provider type %q cannot list models", o.Type)
}

// isMediaCategory reports whether a fal category produces images or video.
// Models without a category are kept, so nothing usable is hidden.
func isMediaCategory(c string) bool {
	return c == "" || strings.HasSuffix(c, "-to-image") || strings.HasSuffix(c, "-to-video")
}

// listModels pages through GET {catalog}/models?status=active&cursor=… ->
// {models: [{endpoint_id, metadata: {display_name, category, description,
// status}}], next_cursor, has_more}. The API key is optional there and
// only raises rate limits.
func (f *fal) listModels(ctx context.Context, catalogURL string) ([]ModelInfo, error) {
	base := strings.TrimRight(strings.TrimSpace(catalogURL), "/")
	if base == "" {
		base = falCatalogURL
	}
	var list []ModelInfo
	seen := map[string]bool{}
	cursor := ""
	for range maxModelPages {
		q := url.Values{"status": {"active"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var out struct {
			Models []struct {
				EndpointID string `json:"endpoint_id"`
				Metadata   *struct {
					DisplayName string `json:"display_name"`
					Category    string `json:"category"`
					Description string `json:"description"`
					Status      string `json:"status"`
				} `json:"metadata"`
			} `json:"models"`
			NextCursor *string `json:"next_cursor"`
			HasMore    bool    `json:"has_more"`
		}
		if err := f.do(ctx, http.MethodGet, base+"/models?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, m := range out.Models {
			info := ModelInfo{ID: m.EndpointID}
			if md := m.Metadata; md != nil {
				if md.Status == "deprecated" {
					continue
				}
				info.Name, info.Category, info.Description = md.DisplayName, md.Category, md.Description
			}
			if info.ID == "" || seen[info.ID] || !isMediaCategory(info.Category) {
				continue
			}
			seen[info.ID] = true
			list = append(list, info)
		}
		if !out.HasMore || out.NextCursor == nil || *out.NextCursor == "" || *out.NextCursor == cursor {
			break
		}
		cursor = *out.NextCursor
		// Be gentle with the unauthenticated rate limit.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.pagePause):
		}
	}
	slices.SortFunc(list, func(a, b ModelInfo) int {
		if c := strings.Compare(a.Category, b.Category); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return list, nil
}
