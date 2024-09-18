PLATFORMS := linux/amd64 darwin/amd64
OUTPUT_DIR := dist

.PHONY: all clean

all: $(PLATFORMS)

$(PLATFORMS):
	GOOS=$(word 1,$(subst /, ,$@)) GOARCH=$(word 2,$(subst /, ,$@)) go build -o $(OUTPUT_DIR)/wapo-prometheus-$(word 1,$(subst /, ,$@))-$(word 2,$(subst /, ,$@)) main.go

clean:
	rm -rf $(OUTPUT_DIR)
