package main

import (
	"fmt"
	"net/http"
)

func metrics(w http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(w, "# HELP stus_metric This is a test metric\n")
	fmt.Fprintf(w, "# TYPE stus_metric gauge\n")
	fmt.Fprintf(w, "stus_metric{namespace=\"my-namespace\",pod=\"my-server\"} 69\n")
}

func main() {
	http.HandleFunc("/metrics", metrics)
	http.ListenAndServe(":8080", nil)
}
