package subsonic

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/server/subsonic/responses"
	"github.com/navidrome/navidrome/utils"
)

func (api *Router) Stream(w http.ResponseWriter, r *http.Request) (*responses.Subsonic, error) {
	ctx := r.Context()
	id, err := requiredParamString(r, "id")
	if err != nil {
		return nil, err
	}
	maxBitRate := utils.ParamInt(r, "maxBitRate", 0)
	format := utils.ParamString(r, "format")
	estimateContentLength := utils.ParamBool(r, "estimateContentLength", false)

	stream, err := api.streamer.NewStream(ctx, id, format, maxBitRate)
	if err != nil {
		return nil, err
	}

	// Make sure the stream will be closed at the end, to avoid leakage
	defer func() {
		if err := stream.Close(); err != nil && log.CurrentLevel() >= log.LevelDebug {
			log.Error(r.Context(), "Error closing stream", "id", id, "file", stream.Name(), err)
		}
	}()

	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Content-Duration", strconv.FormatFloat(float64(stream.Duration()), 'G', -1, 32))

	if stream.Seekable() {
		http.ServeContent(w, r, stream.Name(), stream.ModTime(), stream)
	} else {
		var reqRange = r.Header.Get("Range")
		// "safari or ios range 0-1 informal support, just wait transcode complete and return content length.
		// In next request use seekable data. need enable TranscodingCacheSize."
		if reqRange != "" && strings.HasPrefix(reqRange, "bytes=") {
			startPosition := 0
			endPosition := 0
			reqBlockRange := strings.Split(strings.Split(reqRange, "=")[1], "-")
			startPosition, _ = strconv.Atoi(reqBlockRange[0])
			if len(reqBlockRange) > 1 && reqBlockRange[1] != "" {
				tmp, _ := strconv.Atoi(reqBlockRange[1])
				endPosition = tmp
			}

			if startPosition == 0 && endPosition == 1 {
				data, _ := io.ReadAll(stream)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", startPosition, endPosition, len(data)))
				w.Header().Set("Accept-Ranges", "bytes")
				// w.Header().Set("Transfer-Encoding", "chunked")
				w.Header().Set("Content-Type", "audio/aac")
				w.WriteHeader(206)
				w.Header().Set("Status", "206")
				one := make([]byte, 2)
				// time.Sleep(10 * time.Second)
				// io.ReadFull(stream, one)
				c, err := w.Write(one)
				if log.CurrentLevel() >= log.LevelDebug {
					if err != nil {
						log.Error(ctx, "Error sending range 0-1", "id", id, err)
					} else {
						log.Trace(ctx, "Success sending range 0-1", "id", id, "size", c)
					}
				}
				return nil, nil
			}
		}

		// If the stream doesn't provide a size (i.e. is not seekable), we can't support ranges/content-length
		w.Header().Set("Accept-Ranges", "none")
		w.Header().Set("Content-Type", stream.ContentType())

		// if Client requests the estimated content-length, send it
		if estimateContentLength {
			length := strconv.Itoa(stream.EstimatedContentLength())
			log.Trace(ctx, "Estimated content-length", "contentLength", length)
			w.Header().Set("Content-Length", length)
		}

		if r.Method == "HEAD" {
			go func() { _, _ = io.Copy(io.Discard, stream) }()
		} else {
			c, err := io.Copy(w, stream)
			if log.CurrentLevel() >= log.LevelDebug {
				if err != nil {
					log.Error(ctx, "Error sending transcoded file", "id", id, err)
				} else {
					log.Trace(ctx, "Success sending transcode file", "id", id, "size", c)
				}
			}
		}
	}

	return nil, nil
}

func (api *Router) Download(w http.ResponseWriter, r *http.Request) (*responses.Subsonic, error) {
	ctx := r.Context()
	username, _ := request.UsernameFrom(ctx)
	id, err := requiredParamString(r, "id")
	if err != nil {
		return nil, err
	}

	if !conf.Server.EnableDownloads {
		log.Warn(ctx, "Downloads are disabled", "user", username, "id", id)
		return nil, newError(responses.ErrorAuthorizationFail, "downloads are disabled")
	}

	entity, err := core.GetEntityByID(ctx, api.ds, id)
	if err != nil {
		return nil, err
	}

	setHeaders := func(name string) {
		name = strings.ReplaceAll(name, ",", "_")
		disposition := fmt.Sprintf("attachment; filename=\"%s.zip\"", name)
		w.Header().Set("Content-Disposition", disposition)
		w.Header().Set("Content-Type", "application/zip")
	}

	switch v := entity.(type) {
	case *model.MediaFile:
		stream, err := api.streamer.NewStream(ctx, id, "raw", 0)
		if err != nil {
			return nil, err
		}

		disposition := fmt.Sprintf("attachment; filename=\"%s\"", stream.Name())
		w.Header().Set("Content-Disposition", disposition)
		http.ServeContent(w, r, stream.Name(), stream.ModTime(), stream)
		return nil, nil
	case *model.Album:
		setHeaders(v.Name)
		err = api.archiver.ZipAlbum(ctx, id, w)
	case *model.Artist:
		setHeaders(v.Name)
		err = api.archiver.ZipArtist(ctx, id, w)
	case *model.Playlist:
		setHeaders(v.Name)
		err = api.archiver.ZipPlaylist(ctx, id, w)
	default:
		err = model.ErrNotFound
	}

	if err != nil {
		return nil, err
	}
	return nil, nil
}
