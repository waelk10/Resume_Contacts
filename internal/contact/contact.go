package contact

// Contact holds a scraped email address and associated metadata.
type Contact struct {
	Name   string
	Email  string
	Org    string
	Source string
}
