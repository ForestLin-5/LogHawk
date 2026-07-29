Vagrant.configure("2") do |config|
  config.vm.box = "ubuntu/focal64"

  # Master
  config.vm.define "master" do |m|
    m.vm.hostname = "master"
    m.vm.network "private_network", ip: "192.168.56.10"
    m.vm.provider "virtualbox" do |vb|
      vb.memory = 3072
      vb.cpus = 2
    end
  end

  # Worker 1
  config.vm.define "worker1" do |w|
    w.vm.hostname = "worker1"
    w.vm.network "private_network", ip: "192.168.56.11"
    w.vm.provider "virtualbox" do |vb|
      vb.memory = 2048
      vb.cpus = 1
    end
  end

  # Worker 2
  config.vm.define "worker2" do |w|
    w.vm.hostname = "worker2"
    w.vm.network "private_network", ip: "192.168.56.12"
    w.vm.provider "virtualbox" do |vb|
      vb.memory = 2048
      vb.cpus = 1
    end
  end

  # Common provisioning
  config.vm.provision "shell", inline: <<-SHELL
    # Disable swap (kubeadm requirement)
    swapoff -a
    sed -i '/swap/d' /etc/fstab

    # Install Docker
    curl -fsSL https://get.docker.com | sh
    usermod -aG docker vagrant

    # Install kubeadm/kubelet/kubectl
    curl -s https://packages.cloud.google.com/apt/doc/apt-key.gpg | apt-key add -
    echo "deb https://apt.kubernetes.io/ kubernetes-xenial main" > /etc/apt/sources.list.d/kubernetes.list
    apt-get update
    apt-get install -y kubelet kubeadm kubectl
    apt-mark hold kubelet kubeadm kubectl

    echo ">>> VM ready. 在 Master 上运行: bash scripts/setup-cluster.sh"
  SHELL
end
