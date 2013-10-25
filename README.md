## dengqiao/go-http-proxy-server

`dengqiao/go-http-proxy-server` is a http reverse proxy server write with golang,based on json config

## Requirements
http-proxy-server is write with golang,[so you'll want to have Node installed as well](http://golang.org/doc/install)

## Installation
```sh
$ git clone git clone https://github.com/dengqiao/go-http-proxy-server.git
cp go-http-proxy-server to your gopath 
go build
vi config.json  
add content example
{	
	"Upstreams":[
		{
			"Name":"test",
			"Hosts":["http://localhost:8888","http://localhost:8080"],
			"CheckUrl":"/aliveCheck"
		}
	],
	"Services":[
		{
			"UpstreamName":"test",
			"PathPrefix":"/test"
		}
	]
}

```

## Usage

```
cd go-http-proxy-server path
./go-http-proxy-server --help
./go-http-proxy-server --port=8888
```