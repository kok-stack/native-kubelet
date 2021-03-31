package main

import "net/http"

func main() {
	http.HandleFunc("/index", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("test"))
	})
	http.ListenAndServe(":8080", nil)
}
