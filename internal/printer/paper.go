package printer

import (
	"fmt"
	"math"

	"github.com/OpenPrinting/goipp"
)

func (p PaperSize) valid() bool { return p.Width > 0 && p.Height > 0 }

// Windows rounds dimensions to hundredths of an inch. Use standard PWG names
// when they match within that rounding, and dimension-based names for labels.
func (p PaperSize) keyword() string {
	for _, standard := range []struct {
		name string
		w, h int
	}{
		{"iso_a3_297x420mm", 29700, 42000},
		{"iso_a4_210x297mm", 21000, 29700},
		{"iso_a5_148x210mm", 14800, 21000},
		{"iso_a6_105x148mm", 10500, 14800},
		{"na_letter_8.5x11in", 21590, 27940},
		{"na_legal_8.5x14in", 21590, 35560},
		{"na_index-4x6_4x6in", 10160, 15240},
	} {
		if math.Abs(float64(p.Width-standard.w)) <= 13 && math.Abs(float64(p.Height-standard.h)) <= 13 {
			return standard.name
		}
	}
	return fmt.Sprintf("custom_paper_%gx%gmm", float64(p.Width)/100, float64(p.Height)/100)
}

// Default first: custom USER forms are sometimes absent from PaperSizes.
// Deduplicate dimensions, keeping the default's driver ID when forms overlap.
func (p Printer) papers() []PaperSize {
	var papers []PaperSize
	seen := map[string]bool{}
	for _, paper := range append([]PaperSize{p.DefaultPaper}, p.PaperSizes...) {
		if key := paper.keyword(); paper.valid() && !seen[key] {
			seen[key] = true
			papers = append(papers, paper)
		}
	}
	return papers
}

func paperDimensions(p PaperSize) goipp.Collection {
	return goipp.Collection{
		ippAttr("x-dimension", goipp.TagInteger, goipp.Integer(p.Width)),
		ippAttr("y-dimension", goipp.TagInteger, goipp.Integer(p.Height)),
	}
}

func paperCollection(p PaperSize) goipp.Collection {
	col := goipp.Collection{
		ippAttr("media-size", goipp.TagBeginCollection, paperDimensions(p)),
		ippAttr("media-size-name", goipp.TagKeyword, goipp.String(p.keyword())),
	}
	for _, edge := range []string{"bottom", "left", "right", "top"} {
		col = append(col, ippAttr("media-"+edge+"-margin", goipp.TagInteger, goipp.Integer(0)))
	}
	return col
}

func paperAttributes(p Printer) goipp.Attributes {
	papers := p.papers()
	if len(papers) == 0 {
		// Never invent A4 for a driver whose paper information is unavailable.
		return nil
	}
	names := ippAttr("media-supported", goipp.TagKeyword)
	sizes := ippAttr("media-size-supported", goipp.TagBeginCollection)
	database := ippAttr("media-col-database", goipp.TagBeginCollection)
	for _, paper := range papers {
		names.Values.Add(goipp.TagKeyword, goipp.String(paper.keyword()))
		sizes.Values.Add(goipp.TagBeginCollection, paperDimensions(paper))
		database.Values.Add(goipp.TagBeginCollection, paperCollection(paper))
	}
	attrs := goipp.Attributes{names, sizes, database,
		ippAttr("media-col-supported", goipp.TagKeyword, goipp.String("media-size"), goipp.String("media-size-name"),
			goipp.String("media-bottom-margin"), goipp.String("media-left-margin"), goipp.String("media-right-margin"), goipp.String("media-top-margin")),
	}
	if p.DefaultPaper.valid() {
		attrs = append(attrs,
			ippAttr("media-default", goipp.TagKeyword, goipp.String(p.DefaultPaper.keyword())),
			ippAttr("media-ready", goipp.TagKeyword, goipp.String(p.DefaultPaper.keyword())),
			ippAttr("media-col-default", goipp.TagBeginCollection, paperCollection(p.DefaultPaper)),
			ippAttr("media-col-ready", goipp.TagBeginCollection, paperCollection(p.DefaultPaper)),
		)
	}
	for _, edge := range []string{"bottom", "left", "right", "top"} {
		attrs = append(attrs, ippAttr("media-"+edge+"-margin-supported", goipp.TagInteger, goipp.Integer(0)))
	}
	return attrs
}

func ippCollection(attrs goipp.Attributes, name string) (goipp.Collection, bool) {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) == 1 {
			col, ok := a.Values[0].V.(goipp.Collection)
			return col, ok
		}
	}
	return nil, false
}

// iOS normally selects media with a media-col collection, rather than media.
// Resolve only installed forms, so advertising and printing use the same stock.
func requestedPaper(attrs goipp.Attributes, p Printer) (PaperSize, error) {
	name := ippString(attrs, "media", "")
	w, h := 0, 0
	for _, a := range attrs {
		if a.Name != "media-col" {
			continue
		}
		col, ok := ippCollection(attrs, "media-col")
		if !ok {
			return PaperSize{}, fmt.Errorf("invalid media-col")
		}
		// media-col takes precedence over the legacy media attribute.
		name = ippString(goipp.Attributes(col), "media-size-name", "")
		if size, ok := ippCollection(goipp.Attributes(col), "media-size"); ok {
			w = ippInt(goipp.Attributes(size), "x-dimension", 0)
			h = ippInt(goipp.Attributes(size), "y-dimension", 0)
			if w <= 0 || h <= 0 {
				return PaperSize{}, fmt.Errorf("invalid media dimensions")
			}
		} else if name == "" {
			return PaperSize{}, fmt.Errorf("media-col needs media-size or media-size-name")
		}
		break
	}
	if name == "" && w == 0 && h == 0 {
		return p.DefaultPaper, nil
	}
	for _, paper := range p.papers() {
		if name != "" && name != paper.keyword() {
			continue
		}
		if w != 0 && (math.Abs(float64(w-paper.Width)) > 13 || math.Abs(float64(h-paper.Height)) > 13) {
			continue
		}
		return paper, nil
	}
	return PaperSize{}, fmt.Errorf("requested paper is not supported by %s", p.Name)
}
