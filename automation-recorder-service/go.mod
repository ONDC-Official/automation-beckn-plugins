module automationrecorder

go 1.24.0

// require github.com/beckn-one/beckn-onix v0.0.0-00010101000000-000000000000

replace github.com/beckn-one/beckn-onix => /Users/rudranshsinghal/ondc/automation-utility/official-code/bekn-onix/beckn-onix

replace google.golang.org/protobuf => google.golang.org/protobuf v1.32.0

replace golang.org/x/text => golang.org/x/text v0.26.0

require (
	github.com/alicebob/miniredis/v2 v2.35.0
	github.com/beckn-one/beckn-onix v0.0.0-00010101000000-000000000000
	github.com/joho/godotenv v1.5.1
	github.com/redis/go-redis/v9 v9.17.2
	google.golang.org/grpc v1.78.0
	google.golang.org/protobuf v1.36.10
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/rs/zerolog v1.34.0 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	golang.org/x/net v0.47.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/text v0.32.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251029180050-ab9386a59fda // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)
