package server

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/project"
)

// maxUpload caps the body of an upload request.
const maxUpload = 64 << 20

// ---------------------------------------------------------------------------
// Asset references

// refList resolves a target spec to the slice it designates:
//
//	project            style references of the project
//	visual:<id>        reference images of a visual item
//	visual-out:<id>    generated images of a visual item
//	shot:<id>          explicit reference images of a shot
//	keyframe:<id>      keyframes of a shot
//	endframe:<id>      last-frame images of a shot's clip
//	vref:<id>          reference videos of a shot's clip
//	aref:<id>          reference audio of a shot's clip
//	clip:<id>          video clips of a shot
func refList(p *project.Project, target string) (*[]string, error) {
	kind, id, _ := strings.Cut(target, ":")
	switch kind {
	case "project":
		return &p.ReferenceIDs, nil
	case "visual", "visual-out":
		v := p.Visual(id)
		if v == nil {
			return nil, errors.New("visual item not found")
		}
		if kind == "visual" {
			return &v.ReferenceIDs, nil
		}
		return &v.AssetIDs, nil
	case "shot", "keyframe", "endframe", "vref", "aref", "clip":
		sh := p.Shot(id)
		if sh == nil {
			return nil, errors.New("shot not found")
		}
		switch kind {
		case "shot":
			return &sh.ReferenceIDs, nil
		case "keyframe":
			return &sh.KeyframeIDs, nil
		case "endframe":
			return &sh.EndFrameIDs, nil
		case "vref":
			return &sh.VideoRefIDs, nil
		case "aref":
			return &sh.AudioRefIDs, nil
		}
		return &sh.ClipIDs, nil
	}
	return nil, fmt.Errorf("unknown target %q", target)
}

// targetMedia returns the media type a reference list holds.
func targetMedia(target string) string {
	switch kind, _, _ := strings.Cut(target, ":"); kind {
	case "vref", "clip":
		return "video"
	case "aref":
		return "audio"
	}
	return "image"
}

// checkMedia rejects an asset of the wrong media type for a list.
func checkMedia(a *assets.Asset, target string) error {
	if want := targetMedia(target); a.Media() != want {
		return fmt.Errorf("this list takes %s files", want)
	}
	return nil
}

// handleRefs adds, removes or promotes (op=primary: move to the end, which
// marks it as the one used downstream) an asset in a reference list.
func (s *Server) handleRefs(w http.ResponseWriter, r *http.Request) {
	aid := r.FormValue("asset_id")
	a, err := s.assets.Get(aid)
	if err == nil && r.FormValue("op") == "add" {
		err = checkMedia(a, r.FormValue("target"))
	}
	if err != nil {
		done(w, r, projectURL(r.PathValue("id")), err)
		return
	}
	s.update(w, r, "", func(p *project.Project) error {
		list, err := refList(p, r.FormValue("target"))
		if err != nil {
			return err
		}
		without := slices.DeleteFunc(slices.Clone(*list), func(x string) bool { return x == aid })
		switch r.FormValue("op") {
		case "add", "primary":
			*list = append(without, aid)
		case "remove":
			*list = without
		default:
			return errors.New("unknown op")
		}
		return nil
	})
}

// saveUploads stores multipart files as assets.
func (s *Server) saveUploads(w http.ResponseWriter, r *http.Request, meta func() (assets.Asset, string)) ([]*assets.Asset, error) {
	// Cap the whole request body, and keep at most 8 MB of it in memory
	// (larger files are spooled to temporary files by the parser). This
	// must happen before anything reads a form value.
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, errors.New("upload too large (limit 64 MB)")
		}
		return nil, err
	}
	defer r.MultipartForm.RemoveAll()
	m, want := meta()
	var out []*assets.Asset
	for _, fh := range r.MultipartForm.File["files"] {
		a, err := s.saveUpload(fh, m, want)
		if err != nil {
			return out, fmt.Errorf("%s: %w", fh.Filename, err)
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, errors.New("no files uploaded")
	}
	return out, nil
}

// saveUpload stores one uploaded file. want, if set, is the media type
// ("image", "video", "audio") the file must have.
func (s *Server) saveUpload(fh *multipart.FileHeader, meta assets.Asset, want string) (*assets.Asset, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUpload))
	if err != nil {
		return nil, err
	}
	// Sniff the content. Audio is often not recognised (and an .m4a looks
	// like video/mp4), so there the file name decides.
	mt := http.DetectContentType(data)
	if ext := mime.TypeByExtension(strings.ToLower(filepath.Ext(fh.Filename))); strings.HasPrefix(ext, "audio/") &&
		(mt == "application/octet-stream" || mt == "video/mp4" || strings.HasPrefix(mt, "text/")) {
		mt, _, _ = strings.Cut(ext, ";")
	}
	meta.MIME = mt
	if meta.Media() == "" {
		return nil, fmt.Errorf("unsupported file type %s", mt)
	}
	if want != "" && meta.Media() != want {
		return nil, fmt.Errorf("this list takes %s files", want)
	}
	if meta.Name == "" {
		meta.Name = fh.Filename
	}
	return s.assets.Put(meta, data)
}

func (s *Server) handleProjectUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.projects.Get(id); notFound(w, err) {
		return
	}
	uploaded, err := s.saveUploads(w, r, func() (assets.Asset, string) {
		target := r.FormValue("target")
		meta := assets.Asset{Kind: assets.KindReference, ProjectID: id, Stage: project.StageInput, Tags: []string{"upload"}}
		if strings.HasPrefix(target, "clip:") {
			meta.Kind, meta.Stage = assets.KindVideo, project.StageClips
		}
		return meta, targetMedia(target)
	})
	if len(uploaded) > 0 {
		target := r.FormValue("target")
		_, uerr := s.projects.Update(id, func(p *project.Project) error {
			list, err := refList(p, target)
			if err != nil {
				return err
			}
			for _, a := range uploaded {
				*list = append(*list, a.ID)
			}
			return nil
		})
		if err == nil {
			err = uerr
		}
	}
	done(w, r, projectURL(id), err)
}
