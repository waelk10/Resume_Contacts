package resume

import (
	"fmt"
	"strings"

	"github.com/go-pdf/fpdf"
	"github.com/ledongthuc/pdf"
)

// ExtractText returns the plain text content of every page in a PDF file.
func ExtractText(path string) (string, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", fmt.Errorf("open pdf %q: %w", path, err)
	}
	defer f.Close()

	var sb strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		page := r.Page(i)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			continue
		}
		sb.WriteString(text)
		sb.WriteRune('\n')
	}
	return sb.String(), nil
}

// GeneratePDF renders a plain-text resume (as returned by the Claude tailoring
// call) into a clean A4 PDF saved at outputPath.
//
// Recognised markers:
//
//	"# Name"   → large centred name heading
//	"## SECTION" → bold section heading with a rule underneath
//	""         → paragraph gap
//	everything else → body text
func GeneratePDF(content, outputPath string) error {
	doc := fpdf.New("P", "mm", "A4", "")
	doc.SetMargins(20, 20, 20)
	doc.AddPage()

	const (
		pageW   = 170.0
		bodyHt  = 5.5
		headHt  = 7.0
		nameHt  = 9.0
		paraGap = 3.5
	)

	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, " \t\r")

		switch {
		case strings.HasPrefix(line, "# "):
			doc.SetFont("Helvetica", "B", 18)
			doc.MultiCell(pageW, nameHt, strings.TrimPrefix(line, "# "), "", "C", false)
			doc.Ln(1)

		case strings.HasPrefix(line, "## "):
			doc.Ln(paraGap)
			doc.SetFont("Helvetica", "B", 12)
			doc.MultiCell(pageW, headHt, strings.ToUpper(strings.TrimPrefix(line, "## ")), "", "L", false)
			doc.SetDrawColor(150, 150, 150)
			_, y := doc.GetXY()
			doc.Line(20, y, 20+pageW, y)
			doc.Ln(2)

		case line == "":
			doc.Ln(paraGap)

		default:
			doc.SetFont("Helvetica", "", 10)
			doc.MultiCell(pageW, bodyHt, line, "", "L", false)
		}
	}

	return doc.OutputFileAndClose(outputPath)
}
