#!/bin/sh
set -e

# --- nerdctl containerd run sock path ---
echo 'CONTAINERD_ADDRESS=/run/k8e/containerd/containerd.sock' >> ~/.bashrc 
echo 'export CONTAINERD_ADDRESS' >> ~/.bashrc 


echo 'PATH=$PATH:/usr/local/bin' >> ~/.bashrc 
echo 'export PATH' >> ~/.bashrc 

echo 'alias docker=nerdctl' >> ~/.bashrc

source ~/.bashrc 