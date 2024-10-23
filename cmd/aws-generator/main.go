package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/schidstorm/wg-ondemand/pkg/provision"
)

//go:embed worldcities.csv
var worldCitiesData []byte

type Endpoints struct {
	Partitions []Partition `json:"partitions"`
}

type Partition struct {
	Partition string            `json:"partition"`
	Regions   map[string]Region `json:"regions"`
}

type Region struct {
	Description string `json:"description"`
}

func main() {
	outFile := os.Args[1]

	resp, err := http.DefaultClient.Get("https://raw.githubusercontent.com/boto/botocore/develop/botocore/data/endpoints.json")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	var endpoints Endpoints
	if err := json.NewDecoder(resp.Body).Decode(&endpoints); err != nil {
		panic(err)
	}

	for _, partition := range endpoints.Partitions {
		if partition.Partition != "aws" {
			continue
		}

		var locations []provision.Location
		for region, regionData := range partition.Regions {
			descriptionRegex := `^([^\(]+?)\s*\((.+?)\)$`
			regexp := regexp.MustCompile(descriptionRegex)
			matches := regexp.FindStringSubmatch(regionData.Description)
			var city, country string

			country = matches[1]
			if len(matches) == 3 {
				city = matches[2]
			}

			lat, long := cityToLatitudeLongitude(city)
			locations = append(locations, provision.Location{
				Latitude:  lat,
				Longitude: long,
				Country:   country,
				City:      city,
				Key:       region,
			})
		}

		sort.Slice(locations, func(i, j int) bool {
			return locations[i].Key < locations[j].Key
		})

		err := writeToFile(outFile, genCode(map[string]any{
			"Locations": locations,
		}))
		if err != nil {
			panic(err)
		}
	}
}

func cityToLatitudeLongitude(city string) (float64, float64) {
	lines := bytes.Split(worldCitiesData, []byte("\n"))
	for _, line := range lines {
		// "city","city_ascii","lat","lng","country","iso2","iso3","admin_name","capital","population","id"
		columns := bytes.Split(line, []byte(","))
		const cityColumn = 1
		const countryColumn = 4
		const latitudeColumn = 2
		const longitudeColumn = 3

		if len(columns) < 5 {
			continue
		}

		if strings.EqualFold(strings.Trim(string(columns[cityColumn]), "\""), city) {
			parseFloat := func(s string) float64 {
				f, err := strconv.ParseFloat(strings.Trim(s, "\""), 64)
				if err != nil {
					panic(err)
				}
				return f
			}
			return parseFloat(string(columns[latitudeColumn])), parseFloat(string(columns[longitudeColumn]))
		}

		if strings.EqualFold(strings.Trim(string(columns[countryColumn]), "\""), city) {
			parseFloat := func(s string) float64 {
				f, err := strconv.ParseFloat(strings.Trim(s, "\""), 64)
				if err != nil {
					panic(err)
				}
				return f
			}
			return parseFloat(string(columns[latitudeColumn])), parseFloat(string(columns[longitudeColumn]))
		}
	}

	return 0, 0
}

func genCode(args map[string]any) string {

	tmpl := `
	import (
		"github.com/schidstorm/wg-ondemand/pkg/provision"
	)

	var locations = []provision.Location{
		{{- range $key, $value := .Locations }}
		{
			Latitude:  {{ $value.Latitude }},
			Longitude: {{ $value.Longitude }},
			Country:    "{{ $value.Country }}",
			City:      "{{ $value.City }}",
			Key:       "{{ $value.Key }}",
		},
		{{- end }}
	}
	`

	out := new(bytes.Buffer)
	err := template.Must(template.New("code").Parse(tmpl)).Execute(out, args)
	if err != nil {
		panic(err)
	}
	return out.String()
}

func writeToFile(filename, content string) error {
	fileContent, err := os.ReadFile(filename)
	if err != nil {
		return err
	}

	beginIndex := bytes.Index(fileContent, []byte("//go:generate"))
	if beginIndex == -1 {
		return errors.New("no go:generate directive found")
	}

	for fileContent[beginIndex] != '\n' {
		beginIndex++
	}
	beginIndex++

	fileContent = append(fileContent[:beginIndex], []byte(content)...)
	return os.WriteFile(filename, formatCode(fileContent), 0644)
}

func formatCode(code []byte) []byte {
	cmd := exec.Command("gofmt")
	cmd.Stdin = bytes.NewReader(code)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		panic(err)
	}
	return out.Bytes()
}
