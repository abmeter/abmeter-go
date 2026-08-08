package abmeter

// User is the canonical user value object. UserID is the sole identity and
// randomization unit; Email is optional and only consulted by email-predicate
// audiences — an empty Email is a silent no-match, never an error.
type User struct {
	UserID string
	Email  string
}
