package tools

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// imageHandler is the registry handler for the "image" tool.
func imageHandler(ctx context.Context, args map[string]interface{}) *ToolResult {
	operation, _ := args["operation"].(string)
	if operation == "" {
		operation = "info"
	}
	switch operation {
	case "info":
		return imageInfo(args)
	case "resize":
		return imageResize(ctx, args)
	case "convert":
		return imageConvert(ctx, args)
	default:
		return &ToolResult{Name: "image", Success: false, Error: "unknown operation: " + operation +
			" (valid: info, resize, convert)"}
	}
}

// imageInfo reads lightweight metadata: Image format and dimensions.
func imageInfo(args map[string]interface{}) *ToolResult {
	path, ok := args["file"].(string)
	if !ok || path == "" {
		return &ToolResult{Name: "image", Success: false, Error: "file path required (file)"}
	}

	reader, err := os.Open(path)
	if err != nil {
		return &ToolResult{Name: "image", Success: false, Error: fmt.Sprintf("%s: %v", path, err)}
	}
	defer reader.Close()

	img, format, err := image.DecodeConfig(reader)
	if err != nil {
		return &ToolResult{Name: "image", Success: false, Error: fmt.Sprintf("failed to decode image: %v", err)}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "File: %s\n", path)
	fmt.Fprintf(&sb, "Format: %s\n", format)
	fmt.Fprintf(&sb, "Dimensions: %d × %d\n", img.Width, img.Height)

	return &ToolResult{Name: "image", Success: true, Output: strings.TrimSpace(sb.String())}
}

// imageResize delegates to ImageMagick (convert).
func imageResize(ctx context.Context, args map[string]interface{}) *ToolResult {
	path, ok := args["file"].(string)
	if !ok || path == "" {
		return &ToolResult{Name: "image", Success: false, Error: "file path required (file)"}
	}
	output, _ := args["output"].(string)
	if output == "" {
		return &ToolResult{Name: "image", Success: false, Error: "output path required (output)"}
	}

	width := parseIntArg(args, "width", 0)
	height := parseIntArg(args, "height", 0)

	if width == 0 && height == 0 {
		return &ToolResult{Name: "image", Success: false, Error: "either width or height must be specified"}
	}

	if err := imageRequireIM(); err != nil {
		return &ToolResult{Name: "image", Success: false, Error: err.Error()}
	}

	sizeStr := ""
	if width > 0 && height > 0 {
		sizeStr = fmt.Sprintf("%dx%d!", width, height)
	} else if width > 0 {
		sizeStr = fmt.Sprintf("%d", width)
	} else if height > 0 {
		sizeStr = fmt.Sprintf("x%d", height)
	}

	cmdArgs := []string{path, "-resize", sizeStr, output}
	if out, err := runIM(ctx, cmdArgs); err != nil {
		return &ToolResult{Name: "image", Success: false, Output: out, Error: "imagemagick resize failed: " + err.Error()}
	}

	st, _ := os.Stat(output)
	return &ToolResult{Name: "image", Success: true, Output: fmt.Sprintf(
		"Resized %s → %s (%d bytes)", path, output, st.Size())}
}

// imageConvert delegates to ImageMagick (convert).
func imageConvert(ctx context.Context, args map[string]interface{}) *ToolResult {
	path, ok := args["file"].(string)
	if !ok || path == "" {
		return &ToolResult{Name: "image", Success: false, Error: "file path required (file)"}
	}
	output, _ := args["output"].(string)
	if output == "" {
		return &ToolResult{Name: "image", Success: false, Error: "output path required (output)"}
	}

	if err := imageRequireIM(); err != nil {
		return &ToolResult{Name: "image", Success: false, Error: err.Error()}
	}

	cmdArgs := []string{path, output}
	if out, err := runIM(ctx, cmdArgs); err != nil {
		return &ToolResult{Name: "image", Success: false, Output: out, Error: "imagemagick convert failed: " + err.Error()}
	}

	st, _ := os.Stat(output)
	return &ToolResult{Name: "image", Success: true, Output: fmt.Sprintf(
		"Converted %s → %s (%d bytes)", path, output, st.Size())}
}

// parseIntArg parses an int from the argument map safely.
func parseIntArg(args map[string]interface{}, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func imageRequireIM() error {
	if _, err := exec.LookPath("convert"); err != nil {
		return fmt.Errorf("ImageMagick (convert) is required for this operation but was not found on PATH; install it (e.g. `apt install imagemagick`)")
	}
	return nil
}

func runIM(ctx context.Context, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, "convert", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stderr.String(), err
	}
	return stderr.String(), nil
}
