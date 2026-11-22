package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"

	"github.com/losinggeneration/tau/rpc/client"
)

// uploadAttachments imports each uploaded file into the worker's active
// session through a private temporary file, so file sizes are bounded only by
// Tau, not by the 1 MiB RPC frame that attach_data would need. Tau UI runs as
// the same user and on the same host as the worker, and Tau copies each file
// into its own attachment store before replying.
func uploadAttachments(ctx context.Context, c *client.CoordinatedClient, worker string, files []*multipart.FileHeader) (ids []string, err error) {
	if len(files) == 0 {
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "tau-ui-upload-")
	if err != nil {
		return nil, fmt.Errorf("prepare attachments: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	for i, file := range files {
		name := filepath.Base(file.Filename)
		if name == "." || name == string(filepath.Separator) {
			name = "attachment"
		}
		// A subdirectory per file keeps the original name, which Tau records.
		path := filepath.Join(dir, fmt.Sprint(i), name)
		if err := saveUpload(file, path); err != nil {
			return nil, err
		}
		response, err := workerCommand(ctx, c, worker, "attach_file", map[string]string{"path": path})
		if err != nil {
			return nil, fmt.Errorf("attach %s: %w", file.Filename, err)
		}
		record, _ := response.Result["attachment"].(map[string]any)
		id, _ := record["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("Tau did not return an attachment ID for %s", file.Filename)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func saveUpload(file *multipart.FileHeader, path string) (err error) {
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("prepare %s: %w", file.Filename, err)
	}
	src, err := file.Open()
	if err != nil {
		return fmt.Errorf("open %s: %w", file.Filename, err)
	}
	defer func() { err = errors.Join(err, src.Close()) }()
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("save %s: %w", file.Filename, err)
	}
	defer func() { err = errors.Join(err, dst.Close()) }()
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("save %s: %w", file.Filename, err)
	}
	return nil
}
