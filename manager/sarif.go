package manager

import (
	"fmt"
	"time"
)

//Sarif ...
type Sarif struct {
	Schema  string `json:"$schema"`
	Version string `json:"version"`
	Runs    []Runs `json:"runs"`
}

//ShortDescription ...
type ShortDescription struct {
	Text string `json:"text"`
}

//FullDescription ...
type FullDescription struct {
	Text string `json:"text"`
}

//Rules ...
type Rules struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

//Driver ...
type Driver struct {
	Name            string  `json:"name"`
	SemanticVersion string  `json:"semanticVersion"`
	Rules           []Rules `json:"rules"`
}

//Tool ...
type Tool struct {
	Driver Driver `json:"driver"`
}

//Message ...
type Message struct {
	Text string `json:"text"`
}

//ArtifactLocation ...
type ArtifactLocation struct {
	URI string `json:"uri"`
}

//Region ...
type Region struct {
	StartLine int     `json:"startLine"`
	Snippet   Snippet `json:"snippet"`
}

//Snippet ...
type Snippet struct {
	Text string `json:"text"`
}

//PhysicalLocation ...
type PhysicalLocation struct {
	ArtifactLocation ArtifactLocation `json:"artifactLocation"`
	Region           Region           `json:"region"`
}

//Locations ...
type Locations struct {
	PhysicalLocation PhysicalLocation `json:"physicalLocation"`
}

//Results ...
type Results struct {
	Message    Message          `json:"message"`
	Properties ResultProperties `json:"properties"`
	Locations  []Locations      `json:"locations"`
}

//ResultProperties ...
type ResultProperties struct {
	Commit        string    `json:"commit"`
	Offender      string    `json:"offender"`
	Date          time.Time `json:"date"`
	Author        string    `json:"author"`
	Email         string    `json:"email"`
	CommitMessage string    `json:"commitMessage"`
	Operation     string    `json:"gitOperation"`
	Repo          string    `json:"repo"`
}

//Runs ...
type Runs struct {
	Tool    Tool      `json:"tool"`
	Results []Results `json:"results"`
}

func (manager *Manager) configToRules() []Rules {
	var rules []Rules
	for _, rule := range manager.Config.Rules {
		rules = append(rules, Rules{
			ID:   rule.Description,
			Name: rule.Description,
		})
	}
	return rules
}

func (manager *Manager) leaksToResults() []Results {
	var results []Results
	for _, leak := range manager.leaks {
		results = append(results, Results{
			Message: Message{
				Text: fmt.Sprintf("%s secret detected", leak.Rule),
			},
			Properties: ResultProperties{
				Commit:        leak.Commit,
				Offender:      leak.Offender,
				Date:          leak.Date,
				Author:        leak.Author,
				Email:         leak.Email,
				CommitMessage: leak.Message,
				Operation:     leak.Operation,
				Repo:          leak.Repo,
			},
			Locations: leakToLocation(leak),
		})
	}

	return results
}

func leakToLocation(leak Leak) []Locations {
	return []Locations{
		{
			PhysicalLocation: PhysicalLocation{
				ArtifactLocation: ArtifactLocation{
					URI: leak.File,
				},
				Region: Region{
					StartLine: leak.LineNumber,
					Snippet: Snippet{
						Text: leak.Line,
					},
				},
			},
		},
	}
}
