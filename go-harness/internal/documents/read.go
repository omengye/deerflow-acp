// Package documents extracts passive, bounded document content. Macros, links,
// formula evaluation and embedded objects are never executed or fetched.
package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const MaxBytes = 8 << 20
const MaxText = 4 << 20
const maxCells = 100000

type Table struct {
	Name string     `json:"name"`
	Rows [][]string `json:"rows"`
}
type Document struct {
	Text   string  `json:"text"`
	Tables []Table `json:"tables,omitempty"`
}

type boundedText struct{ builder *strings.Builder }

func (w boundedText) Write(p []byte) (int, error) {
	if w.builder.Len()+len(p) > MaxText {
		return 0, errors.New("extracted workbook exceeds 4 MiB")
	}
	return w.builder.Write(p)
}

func MIME(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".pdf":
		return "application/pdf"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	}
	return ""
}

func Extract(ctx context.Context, name string, data []byte) (Document, error) {
	if len(data) > MaxBytes {
		return Document{}, errors.New("document exceeds 8 MiB")
	}
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".pdf":
		text, err := pdfText(ctx, data)
		return Document{Text: text}, err
	case ".docx", ".xlsx":
		files, err := officeFiles(ctx, data)
		if err != nil {
			return Document{}, err
		}
		if strings.EqualFold(path.Ext(name), ".docx") {
			body, ok := files["word/document.xml"]
			if !ok {
				return Document{}, errors.New("missing DOCX document")
			}
			text, err := xmlText(ctx, body, true)
			return Document{Text: text}, err
		}
		return workbook(ctx, files)
	case ".csv", ".tsv":
		if len(data) > MaxText {
			return Document{}, errors.New("table exceeds 4 MiB")
		}
		if !utf8.Valid(data) {
			return Document{}, errors.New("table must use UTF-8")
		}
		r := csv.NewReader(bytes.NewReader(data))
		r.FieldsPerRecord = -1
		if strings.EqualFold(path.Ext(name), ".tsv") {
			r.Comma = '\t'
		}
		t := Table{Name: name}
		cells := 0
		for {
			if err := ctx.Err(); err != nil {
				return Document{}, err
			}
			row, err := r.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return Document{}, err
			}
			cells += len(row)
			if cells > maxCells {
				return Document{}, errors.New("table exceeds 100000 cells")
			}
			t.Rows = append(t.Rows, row)
		}
		return Document{Text: string(data), Tables: []Table{t}}, nil
	default:
		return Document{}, errors.New("supported documents: PDF, DOCX, XLSX, CSV, TSV")
	}
}

func officeFiles(ctx context.Context, data []byte) (map[string][]byte, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("invalid or encrypted Office file")
	}
	if len(z.File) > 2048 {
		return nil, errors.New("Office archive has too many entries")
	}
	files := map[string][]byte{}
	total := 0
	for _, f := range z.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(f.Name, ".xml") && !strings.HasSuffix(f.Name, ".rels") {
			continue
		}
		if !utf8.ValidString(f.Name) || strings.Contains(f.Name, "\\") || strings.HasPrefix(f.Name, "/") || path.Clean(f.Name) != f.Name || strings.HasPrefix(f.Name, "../") {
			return nil, errors.New("invalid Office entry name")
		}
		if _, ok := files[f.Name]; ok {
			return nil, errors.New("duplicate Office entry")
		}
		if f.UncompressedSize64 > MaxText || uint64(total)+f.UncompressedSize64 > 32<<20 {
			return nil, errors.New("Office XML expansion exceeds limit")
		}
		r, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(r, MaxText+1))
		closeErr := r.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(b) > MaxText {
			return nil, errors.New("Office XML expansion exceeds limit")
		}
		total += len(b)
		if total > 32<<20 {
			return nil, errors.New("Office XML expansion exceeds limit")
		}
		files[f.Name] = b
	}
	return files, nil
}

func xmlText(ctx context.Context, data []byte, paragraphs bool) (string, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	var out strings.Builder
	textDepth := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		t, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch v := t.(type) {
		case xml.StartElement:
			if v.Name.Local == "t" {
				textDepth++
			}
			if paragraphs && v.Name.Local == "tab" {
				out.WriteByte('\t')
			}
		case xml.CharData:
			if textDepth > 0 {
				out.Write(v)
			}
		case xml.EndElement:
			if v.Name.Local == "t" {
				textDepth--
			}
			if paragraphs && (v.Name.Local == "p" || v.Name.Local == "br" || v.Name.Local == "tr") {
				out.WriteByte('\n')
			}
		}
		if out.Len() > MaxText {
			return "", errors.New("extracted document exceeds 4 MiB")
		}
	}
	return out.String(), nil
}

func workbook(ctx context.Context, files map[string][]byte) (Document, error) {
	var shared []string
	if b := files["xl/sharedStrings.xml"]; len(b) > 0 {
		d := xml.NewDecoder(bytes.NewReader(b))
		for {
			if err := ctx.Err(); err != nil {
				return Document{}, err
			}
			t, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return Document{}, err
			}
			if s, ok := t.(xml.StartElement); ok && s.Name.Local == "si" {
				var item struct {
					Inner string `xml:",innerxml"`
				}
				if err = d.DecodeElement(&item, &s); err != nil {
					return Document{}, err
				}
				text, err := xmlText(ctx, []byte("<root>"+item.Inner+"</root>"), false)
				if err != nil {
					return Document{}, err
				}
				shared = append(shared, text)
				if len(shared) > maxCells {
					return Document{}, errors.New("too many shared strings")
				}
			}
		}
	}
	var book struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			ID   string `xml:"id,attr"`
		} `xml:"sheets>sheet"`
	}
	var rels struct {
		Items []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
			Mode   string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(files["xl/workbook.xml"], &book); err != nil {
		return Document{}, err
	}
	if err := xml.Unmarshal(files["xl/_rels/workbook.xml.rels"], &rels); err != nil {
		return Document{}, err
	}
	if len(book.Sheets) > 64 {
		return Document{}, errors.New("workbook exceeds 64 sheets")
	}
	targets := map[string]string{}
	for _, r := range rels.Items {
		if r.Mode != "External" {
			target := path.Clean(path.Join("xl", r.Target))
			if strings.HasPrefix(r.Target, "/") {
				target = strings.TrimPrefix(r.Target, "/")
			}
			targets[r.ID] = target
		}
	}
	out := Document{}
	cells := 0
	var text strings.Builder
	text.WriteString("XLSX values are cached values only; formulas are not evaluated. Dates may be Excel serials.\n")
	for _, s := range book.Sheets {
		b, ok := files[targets[s.ID]]
		if !ok {
			return Document{}, errors.New("missing or external worksheet")
		}
		d := xml.NewDecoder(bytes.NewReader(b))
		table := Table{Name: s.Name}
		for {
			if err := ctx.Err(); err != nil {
				return Document{}, err
			}
			t, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return Document{}, err
			}
			if row, ok := t.(xml.StartElement); ok && row.Name.Local == "row" {
				var r struct {
					Cells []struct {
						Ref    string `xml:"r,attr"`
						Type   string `xml:"t,attr"`
						Value  string `xml:"v"`
						Inline struct {
							Inner string `xml:",innerxml"`
						} `xml:"is"`
					} `xml:"c"`
				}
				if err = d.DecodeElement(&r, &row); err != nil {
					return Document{}, err
				}
				values := []string{}
				for _, c := range r.Cells {
					col := 0
					for _, ch := range c.Ref {
						if ch < 'A' || ch > 'Z' {
							break
						}
						col = col*26 + int(ch-'A') + 1
						if col > 1024 {
							return Document{}, errors.New("worksheet exceeds 1024 columns")
						}
					}
					if col == 0 {
						col = len(values) + 1
					}
					if col <= len(values) {
						return Document{}, errors.New("unordered or duplicate worksheet cells")
					}
					cells += col - len(values)
					if cells > maxCells {
						return Document{}, errors.New("workbook exceeds 100000 cells")
					}
					for len(values) < col {
						values = append(values, "")
					}
					value := c.Value
					if c.Type == "s" {
						i, err := strconv.Atoi(value)
						if err != nil || i < 0 || i >= len(shared) {
							return Document{}, errors.New("invalid shared string index")
						}
						value = shared[i]
					}
					if c.Type == "inlineStr" {
						var err error
						value, err = xmlText(ctx, []byte("<root>"+c.Inline.Inner+"</root>"), false)
						if err != nil {
							return Document{}, err
						}
					}
					values[col-1] = value
				}
				table.Rows = append(table.Rows, values)
				if len(table.Rows) > maxCells {
					return Document{}, errors.New("too many worksheet rows")
				}
			}
		}
		if _, err := io.WriteString(boundedText{&text}, "\nSheet: "+s.Name+"\n"); err != nil {
			return Document{}, err
		}
		w := csv.NewWriter(boundedText{&text})
		w.Comma = '\t'
		for _, row := range table.Rows {
			if err := w.Write(row); err != nil {
				return Document{}, err
			}
			w.Flush()
			if err := w.Error(); err != nil {
				return Document{}, err
			}
			if text.Len() > MaxText {
				return Document{}, errors.New("extracted workbook exceeds 4 MiB")
			}
		}
		out.Tables = append(out.Tables, table)
	}
	out.Text = text.String()
	return out, nil
}

type ColumnStats struct {
	Column  int      `json:"column"`
	Name    string   `json:"name"`
	Count   int      `json:"count"`
	Missing int      `json:"missing"`
	Numeric int      `json:"numeric"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Mean    *float64 `json:"mean,omitempty"`
	Sum     *float64 `json:"sum,omitempty"`
}
type TableStats struct {
	Name    string        `json:"name"`
	Rows    int           `json:"rows"`
	Columns []ColumnStats `json:"columns"`
}

func Analyze(doc Document, header bool) ([]TableStats, error) {
	if len(doc.Tables) == 0 {
		return nil, errors.New("attachment has no tables; use CSV, TSV or XLSX")
	}
	result := []TableStats{}
	for _, t := range doc.Tables {
		rows := t.Rows
		width := 0
		for _, r := range rows {
			width = max(width, len(r))
		}
		if width > 256 {
			return nil, errors.New("analysis supports at most 256 columns per sheet")
		}
		stats := TableStats{Name: t.Name, Columns: make([]ColumnStats, width)}
		for i := range stats.Columns {
			stats.Columns[i] = ColumnStats{Column: i + 1, Name: fmt.Sprintf("column_%d", i+1)}
			if header && len(rows) > 0 && i < len(rows[0]) {
				stats.Columns[i].Name = rows[0][i]
			}
		}
		if header && len(rows) > 0 {
			rows = rows[1:]
		}
		stats.Rows = len(rows)
		for i := range stats.Columns {
			c := &stats.Columns[i]
			numbers := []float64{}
			for _, row := range rows {
				if i >= len(row) || strings.TrimSpace(row[i]) == "" {
					c.Missing++
					continue
				}
				c.Count++
				n, err := strconv.ParseFloat(strings.TrimSpace(row[i]), 64)
				if err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
					numbers = append(numbers, n)
				}
			}
			c.Numeric = len(numbers)
			if len(numbers) > 0 {
				sort.Float64s(numbers)
				sum := 0.0
				mean := 0.0
				for _, n := range numbers {
					sum += n
					mean += n / float64(len(numbers))
				}
				if math.IsInf(sum, 0) || math.IsNaN(sum) || math.IsInf(mean, 0) || math.IsNaN(mean) {
					return nil, errors.New("numeric aggregate overflow")
				}
				lo, hi := numbers[0], numbers[len(numbers)-1]
				c.Min, c.Max, c.Mean, c.Sum = &lo, &hi, &mean, &sum
			}
		}
		result = append(result, stats)
	}
	return result, nil
}
