module networkobservability

go 1.25.5

// require github.com/beckn-one/beckn-onix v0.0.0-00010101000000-000000000000

replace github.com/beckn-one/beckn-onix => /Users/rudranshsinghal/ondc/automation-utility/official-code/bekn-onix/beckn-onix

replace validationpkg => ./validationpkg

require github.com/beckn-one/beckn-onix v0.0.0-00010101000000-000000000000

require github.com/AsaiYusuke/jsonpath v1.6.0

require gopkg.in/yaml.v3 v3.0.1

require (
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/rs/zerolog v1.34.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)
