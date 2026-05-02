package server

import "context"

// ListImages retrieves the list of available server images via
// "server/list_images.json".
func (s *Client) ListImages(ctx context.Context) (response ListImagesResponse, err error) {
	u := "server/list_images.json"

	req, err := s.client.NewRequest("GET", u, "")
	if err != nil {
		return response, err
	}

	if err := s.client.Do(ctx, req, &response); err != nil {
		return response, err
	}

	return response, nil
}
