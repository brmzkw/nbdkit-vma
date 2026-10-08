package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"vma-fuse-poc/internal/vma"
)

type jsonConfig struct {
	Name string `json:"name"`
	Size int    `json:"size_bytes"`
	Text string `json:"text,omitempty"`
}

type jsonDevice struct {
	ID   int    `json:"dev_id"`
	Name string `json:"name"`
	Size uint64 `json:"size_bytes"`
}

type jsonOutput struct {
	Ctime   string       `json:"ctime"`
	UUID    string       `json:"uuid"`
	Devices []jsonDevice `json:"devices"`
	Configs []jsonConfig `json:"configs"`
}

func printHuman(path string, hdr *vma.Header) {
	fmt.Printf("%s\n", path)
	fmt.Printf("  uuid:  %x\n", hdr.UUID)
	fmt.Printf("  ctime: %s\n\n", time.Unix(int64(hdr.CtimeUnix), 0).UTC().Format(time.RFC3339))

	fmt.Println("Devices (dev-name is the identifier to pass to -m):")
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  DEV_ID\tNAME\tSIZE\tNOTE")
	for _, d := range hdr.Devices {
		note := ""
		if d.Name == "vmstate" {
			note = "live RAM state snapshot, not a disk"
		}
		_, _ = fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\n", d.ID, d.Name, humanSize(d.Size), note)
	}
	_ = tw.Flush()
	if len(hdr.Devices) == 0 {
		fmt.Println("  (none found)")
	}

	fmt.Println("\nEmbedded config blobs:")
	for _, c := range hdr.Configs {
		fmt.Printf("  --- %s (%d bytes) ---\n", c.Name, len(c.Data))
		if isPrintableText(c.Data) {
			fmt.Println(indent(string(c.Data)))
		} else {
			fmt.Println("  (binary content, not shown)")
		}
	}
	if len(hdr.Configs) == 0 {
		fmt.Println("  (none found)")
	}

	fmt.Println("\nNote: guest filesystem type/layout inside each disk (partition table, ext4 vs" +
		" xfs vs LVM, etc.) is NOT determinable from the VMA header alone -- that requires reading" +
		" guest data, which is exactly what -m + guestmount do.")
}

func printJSON(hdr *vma.Header) {
	out := jsonOutput{
		Ctime: time.Unix(int64(hdr.CtimeUnix), 0).UTC().Format(time.RFC3339),
		UUID:  fmt.Sprintf("%x", hdr.UUID),
	}
	for _, d := range hdr.Devices {
		out.Devices = append(out.Devices, jsonDevice{ID: d.ID, Name: d.Name, Size: d.Size})
	}
	for _, c := range hdr.Configs {
		jc := jsonConfig{Name: c.Name, Size: len(c.Data)}
		if isPrintableText(c.Data) {
			jc.Text = string(c.Data)
		}
		out.Configs = append(out.Configs, jc)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func humanSize(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func isPrintableText(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	for _, c := range b {
		if c == '\n' || c == '\t' || c == '\r' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func indent(s string) string {
	var out strings.Builder
	out.WriteString("  ")
	for _, r := range s {
		out.WriteRune(r)
		if r == '\n' {
			out.WriteString("  ")
		}
	}
	return out.String()
}
