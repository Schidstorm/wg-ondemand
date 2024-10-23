customQualifier = c762bc03

install: build
	sudo cp ./bin/wg-ondemand /usr/local/bin/

build: generate go_build

go_build: 
	go build -ldflags="-X 'github.com/schidstorm/wg-ondemand/pkg/aws.buildArgCustomQualifier=$(customQualifier)'" -o bin/wg-ondemand ./cmd/wg-ondemand

generate: generateBootstrap generateCdk go_generate

go_generate:
	if ! [ -f cmd/aws-generator/worldcities.csv ]; then curl https://simplemaps.com/static/data/world-cities/basic/simplemaps_worldcities_basicv1.77.zip | busybox unzip -p - worldcities.csv > cmd/aws-generator/worldcities.csv; fi && \
	go generate ./...

generateCdk:
	cd pkg/aws/cdk && \
	rm -rf cdk.out ../cdk.out && \
	cdk synth --termination-protection=false | sed 's/hnb659fds/$(customQualifier)/g' > ../cdk.template.yaml && \
	find cdk.out -name '*.json' -exec bash -c 'sed -i "s/hnb659fds/$(customQualifier)/g" {} && echo {}' \; && \
	cp -ar cdk.out ../cdk.out

clean:
	rm -rf pkg/aws/cdk/cdk.out pkg/aws/cdk.out && \
	rm -rf bin && \
	rm -f pkg/aws/bootstrap.template.yaml pkg/aws/cdk.template.yaml && \
	rm -f cmd/aws-generator/worldcities.csv
	mkdir bin

generateBootstrap:
	cd pkg/aws/cdk && cdk bootstrap --termination-protection=false --show-template | sed 's/hnb659fds/$(customQualifier)/g' > ../bootstrap.template.yaml