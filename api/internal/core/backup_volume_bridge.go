package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

const (
	volumeTransferTimeout = 70 * time.Second
	volumeTransferStatus  = "Kamui-Volume-Transfer"
)

type bridgeVolumeBackend interface {
	BackupVolume(context.Context, BackupVolumeRef, string, io.Writer) error
	RestoreVolume(context.Context, BackupVolumeRef, string, string, io.Reader) error
	CleanupRestoreVolume(context.Context, BackupVolumeRef, string, string) error
}

func registerVolumeBridge(mux *http.ServeMux, backend bridgeVolumeBackend) {
	mux.HandleFunc("POST /internal/core/docker/volume/{operation}", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), volumeTransferTimeout)
		defer cancel()
		deadline, _ := ctx.Deadline()
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			http.Error(w, "transfer unavailable", 503)
			return
		}
		if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			http.Error(w, "transfer unavailable", 503)
			return
		}
		values := r.URL.Query()
		ref := BackupVolumeRef{ProjectID: values.Get("project"), Name: values.Get("volume")}
		engine, operation := values.Get("engine"), values.Get("restore")
		var err error
		switch r.PathValue("operation") {
		case "export":
			w.Header().Set("Trailer", volumeTransferStatus)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			err = backend.BackupVolume(ctx, ref, engine, w)
			w.Header().Set(volumeTransferStatus, volumeTransferCode(err))
			return
		case "restore":
			r.Body = http.MaxBytesReader(w, r.Body, maxVolumeArchive)
			err = backend.RestoreVolume(ctx, ref, engine, operation, r.Body)
		case "cleanup":
			err = backend.CleanupRestoreVolume(ctx, ref, engine, operation)
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set(volumeTransferStatus, volumeTransferCode(err))
		w.WriteHeader(http.StatusNoContent)
	})
}

func volumeTransferCode(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrBackupVolumeNotFound):
		return "not_found"
	case errors.Is(err, ErrBackupVolumeConflict):
		return "conflict"
	case errors.Is(err, ErrBackupVolumeLocality):
		return "locality"
	case errors.Is(err, ErrBackupVolumeArchive):
		return "archive"
	default:
		slog.Warn("managed volume transfer failed", "error", err)
		return "failed"
	}
}

func volumeTransferError(code string) error {
	switch code {
	case "ok":
		return nil
	case "not_found":
		return ErrBackupVolumeNotFound
	case "conflict":
		return ErrBackupVolumeConflict
	case "locality":
		return ErrBackupVolumeLocality
	case "archive":
		return ErrBackupVolumeArchive
	default:
		return errors.New("managed volume transfer did not complete")
	}
}

func (b *Bridge) transferVolume(ctx context.Context, op string, ref BackupVolumeRef, engine, operation string, archive io.Reader, sink io.Writer) error {
	values := url.Values{"project": {ref.ProjectID}, "volume": {ref.Name}, "engine": {engine}, "restore": {operation}}
	req, err := http.NewRequestWithContext(ctx, "POST", b.baseURL+"/internal/core/docker/volume/"+op+"?"+values.Encode(), archive)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	cli := *b.client
	cli.Timeout = volumeTransferTimeout
	res, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if sink != nil {
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("volume bridge responded with HTTP %d", res.StatusCode)
		}
		if _, err := io.Copy(sink, res.Body); err != nil {
			return err
		}
		return volumeTransferError(res.Trailer.Get(volumeTransferStatus))
	}
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("volume bridge responded with HTTP %d", res.StatusCode)
	}
	return volumeTransferError(res.Header.Get(volumeTransferStatus))
}

func (b *Bridge) BackupVolume(ctx context.Context, ref BackupVolumeRef, engine string, sink io.Writer) error {
	return b.transferVolume(ctx, "export", ref, engine, "", nil, sink)
}

func (b *Bridge) RestoreVolume(ctx context.Context, ref BackupVolumeRef, engine, operation string, archive io.Reader) error {
	return b.transferVolume(ctx, "restore", ref, engine, operation, archive, nil)
}

func (b *Bridge) CleanupRestoreVolume(ctx context.Context, ref BackupVolumeRef, engine, operation string) error {
	return b.transferVolume(ctx, "cleanup", ref, engine, operation, nil, nil)
}
