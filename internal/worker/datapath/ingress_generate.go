package datapath

//go:generate go tool bpf2go -cc bpf-clang -no-strip -target bpfel -type binding_state ingress ingress_bpf.c
