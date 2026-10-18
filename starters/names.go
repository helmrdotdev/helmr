package starters

import (
	"math/rand/v2"
	"regexp"
)

// Namesake is a default Agent name and the person it honors.
type Namesake struct {
	Name   string
	Person string
	Field  string
}

// The default names: deceased scientists and mathematicians, balanced by
// gender and region, short enough to mention in Slack. Keep the list frozen
// once released; add a name only with an equally notable counterpart.
var namesakes = []Namesake{
	{Name: "noether", Person: "Emmy Noether", Field: "mathematician"},
	{Name: "meitner", Person: "Lise Meitner", Field: "physicist"},
	{Name: "leavitt", Person: "Henrietta Swan Leavitt", Field: "astronomer"},
	{Name: "tharp", Person: "Marie Tharp", Field: "ocean cartographer"},
	{Name: "easley", Person: "Annie Easley", Field: "computer scientist"},
	{Name: "saruhashi", Person: "Katsuko Saruhashi", Field: "geochemist"},
	{Name: "agnesi", Person: "Maria Gaetana Agnesi", Field: "mathematician"},
	{Name: "janaki", Person: "E. K. Janaki Ammal", Field: "botanist"},
	{Name: "somerville", Person: "Mary Somerville", Field: "scientist"},
	{Name: "maathai", Person: "Wangari Maathai", Field: "biologist"},
	{Name: "bowditch", Person: "Nathaniel Bowditch", Field: "navigator and mathematician"},
	{Name: "harriot", Person: "Thomas Harriot", Field: "mathematician and astronomer"},
	{Name: "shenkuo", Person: "Shen Kuo", Field: "polymath"},
	{Name: "khwarizmi", Person: "Muhammad ibn Musa al-Khwarizmi", Field: "mathematician"},
	{Name: "haytham", Person: "Ibn al-Haytham", Field: "physicist"},
	{Name: "biruni", Person: "Abu Rayhan al-Biruni", Field: "scholar"},
	{Name: "seki", Person: "Seki Takakazu", Field: "mathematician"},
	{Name: "aryabhata", Person: "Aryabhata", Field: "mathematician and astronomer"},
	{Name: "banneker", Person: "Benjamin Banneker", Field: "astronomer"},
	{Name: "laplace", Person: "Pierre-Simon Laplace", Field: "mathematician"},
}

var agentNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// ValidAgentName reports whether name works as an Agent ID, a file name and a
// Slack mention.
func ValidAgentName(name string) bool {
	return agentNamePattern.MatchString(name)
}

// RandomName picks a default Agent name.
func RandomName() string {
	return namesakes[rand.IntN(len(namesakes))].Name
}

// LookupNamesake returns the person a default name honors.
func LookupNamesake(name string) (Namesake, bool) {
	for _, namesake := range namesakes {
		if namesake.Name == name {
			return namesake, true
		}
	}
	return Namesake{}, false
}
