#! /bin/sh
eval "$(ssh-agent -s)"
ssh-add ~/.ssh/cloudlab_rsa
ssh-add ~/.ssh/id_ed25519
export SSH_AUTH_SOCK

username="ethanxu"
hosts=(
  "hp066.utah.cloudlab.us" 
  "hp055.utah.cloudlab.us" 
  "hp062.utah.cloudlab.us"
  "hp051.utah.cloudlab.us" 
  "hp050.utah.cloudlab.us" 
  "hp056.utah.cloudlab.us"
  "hp064.utah.cloudlab.us" 
  "hp065.utah.cloudlab.us"
)
private_ips=(
  "10.10.1.1" 
  "10.10.1.2" 
  "10.10.1.3"
  "10.10.1.4" 
  "10.10.1.5" 
  "10.10.1.6"
  "10.10.1.7" 
  "10.10.1.8" 
)
raft_ports=(
  "11379" 
  "12379" 
  "13379"
  "14379" 
  "15379" 
  "16379"
  "17379" 
  "18379" 
)

deploy_hostlist=("${hosts[@]}")
echo "deploying to: ${deploy_hostlist[@]}"

# Commands functions
function kill_raft() {
    for i in "${!deploy_hostlist[@]}";
    do 
      hostname="${deploy_hostlist[$i]}"
      raft_port="${raft_ports[$i]}"
      (
        echo "Killing Raft on host with username: ${username}, hostname: ${hostname}, raft port: ${raft_port}"
        ssh -A -v -n -o BatchMode=yes -o StrictHostKeyChecking=no ${username}@${hostname} "export CLUSTER_ID=${cluster_id}; export PRIVATE_IPS=${private_ips[@]}; export RAFT_PORT=${raft_port}; $1; $2;"
      ) &
    done

    wait
}

commands=(
  # kill user process running on $RAFT_PORT (potentially old raft instance)
  "sudo fuser -n tcp -k \$RAFT_PORT"

  # delete snapshots so that cluster starts freshly from term 0
  "./clear_snap.sh"
)

kill_raft "${commands[@]}"