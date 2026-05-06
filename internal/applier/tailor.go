package applier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// errNoJobDescription is returned when Claude signals the job description is
// too thin to tailor against; the caller should fall back to the original resume.
var errNoJobDescription = errors.New("job description insufficient for tailoring")

// buildTailorPrompt returns a single, self-contained prompt with all inputs
// clearly delimited so the claude CLI needs no context beyond the text itself.
func buildTailorPrompt(baseResume, jobDesc, jobTitle, company string) string {
	return fmt.Sprintf(`Rewrite the resume inside <resume> tags to be precisely tailored to the job posting inside <job_posting> tags.

IMPORTANT: If the <job_posting> section is empty, missing, or does not contain
enough detail to make meaningful tailoring decisions, output ONLY the exact word:
FALLBACK
Do not output anything else in that case.

Otherwise follow these rules:
- Do NOT invent, exaggerate, or omit any factual information.
- Reorder and reword bullet points to lead with the experience most relevant to
  this role, mirroring key terms from the job description where they honestly apply.
- Tighten language; remove filler phrases.
- Return ONLY the resume text — no preamble, no commentary, no markdown fences.

Output format (use these exact heading markers, nothing else):
# Full Name
contact line: email | phone | LinkedIn | GitHub  (omit absent fields)

## PROFESSIONAL SUMMARY
2–3 sentences tailored to this specific role and company.

## SKILLS
Comma-separated list ordered by relevance to this role.

## EXPERIENCE
For each role:
**Job Title — Company**  (Month Year – Month Year or Present)
- Achievement or responsibility, quantified where possible
- …

## EDUCATION
Degree, Institution, Year

Add further sections (Certifications, Projects, Publications, …) only when genuinely relevant.

<job_posting>
Job title: %s
Company: %s

%s
</job_posting>

<resume>
%s
</resume>`, jobTitle, company, jobDesc, baseResume)
}

// TailorResume invokes the local claude CLI in print mode with a fully
// self-contained prompt and returns the tailored resume as plain text.
// Returns errNoJobDescription when the job description is too thin to tailor
// against — callers should fall back to the original resume file.
func TailorResume(
	ctx context.Context,
	baseResume, jobDesc, jobTitle, company string,
) (string, error) {
	prompt := buildTailorPrompt(baseResume, jobDesc, jobTitle, company)

	cmd := exec.CommandContext(ctx, "claude", "--print")
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude CLI: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	result := strings.TrimSpace(stdout.String())
	if result == "" {
		return "", fmt.Errorf("claude CLI returned an empty response")
	}
	if result == "FALLBACK" {
		return "", errNoJobDescription
	}
	return result, nil
}
