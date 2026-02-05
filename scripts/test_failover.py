#!/usr/bin/env python3
"""
MonoFS Failover & Rebalancing Test Suite

This script tests various failover scenarios to ensure:
1. Repository discovery works when router starts
2. Node recovery syncs missing files to recovered nodes
3. Rebalancing redistributes files correctly
4. Cleanup removes old file copies after grace period
5. Filesystem remains accessible during failover

Usage:
    python3 scripts/test_failover.py [--quick] [--skip-fs]
    
    --quick: Use shorter grace periods for faster testing
    --skip-fs: Skip filesystem mount/access tests
"""

import subprocess
import time
import json
import sys
import os
import re
import tempfile
import shutil
from dataclasses import dataclass
from typing import Dict, List, Optional, Tuple
from datetime import datetime


@dataclass
class NodeStatus:
    node_id: str
    owned_files: int
    healthy: bool
    repos: int


@dataclass 
class FSStatus:
    mounted: bool
    repos_visible: int
    files_accessible: int
    read_errors: int
    mount_point: str


@dataclass
class TestResult:
    name: str
    passed: bool
    message: str
    duration: float


class MonoFSTestSuite:
    def __init__(self, project_dir: str = "/home/rydzu/projects/monofs"):
        self.project_dir = project_dir
        self.results: List[TestResult] = []
        self.nodes = ["node1", "node2", "node3", "node4", "node5"]
        self.routers = ["router1", "router2"]
        self.mount_point = "/tmp/monofs-test-mount"
        self.fuse_pid: Optional[int] = None
        
    def run_command(self, cmd: str, timeout: int = 60) -> Tuple[int, str, str]:
        """Run a shell command and return exit code, stdout, stderr."""
        try:
            result = subprocess.run(
                cmd,
                shell=True,
                cwd=self.project_dir,
                capture_output=True,
                text=True,
                timeout=timeout
            )
            return result.returncode, result.stdout, result.stderr
        except subprocess.TimeoutExpired:
            return -1, "", "Command timed out"
    
    def log(self, msg: str, level: str = "INFO"):
        """Print a log message with timestamp."""
        timestamp = datetime.now().strftime("%H:%M:%S")
        colors = {
            "INFO": "\033[36m",    # Cyan
            "OK": "\033[32m",      # Green
            "FAIL": "\033[31m",    # Red
            "WARN": "\033[33m",    # Yellow
            "TEST": "\033[35m",    # Magenta
            "FS": "\033[94m",      # Light Blue
        }
        reset = "\033[0m"
        color = colors.get(level, "")
        print(f"{color}[{timestamp}] [{level}] {msg}{reset}")
    
    def docker_compose(self, action: str, services: List[str] = None, timeout: int = 120) -> bool:
        """Run docker compose command."""
        cmd = f"docker compose {action}"
        if services:
            cmd += " " + " ".join(services)
        code, stdout, stderr = self.run_command(cmd, timeout=timeout)
        if code != 0:
            self.log(f"Docker compose failed: {stderr}", "FAIL")
            return False
        return True
    
    def get_router_logs(self, router: str = "router1", lines: int = 100) -> str:
        """Get recent logs from a router."""
        _, stdout, _ = self.run_command(f"docker compose logs {router} --tail={lines}")
        return stdout
    
    def get_node_file_counts(self) -> Dict[str, NodeStatus]:
        """Parse router logs to get current node file counts."""
        logs = self.get_router_logs(lines=100)
        node_status = {}
        
        # Method 1: Parse logs like: node_id=node1 owned_files=1459 cluster_repos=2
        pattern = r'node_id=(\w+) owned_files=(\d+) cluster_repos=(\d+)'
        for match in re.finditer(pattern, logs):
            node_id = match.group(1)
            owned_files = int(match.group(2))
            cluster_repos = int(match.group(3))
            node_status[node_id] = NodeStatus(
                node_id=node_id,
                owned_files=owned_files,
                healthy=True,
                repos=cluster_repos
            )
        
        # Method 2: If no file counts, check for node_id mentions in health/recovery logs
        # This handles fresh cluster with no repos yet
        if not node_status:
            node_pattern = r'node_id=(node\d+)'
            found_nodes = set(re.findall(node_pattern, logs))
            for node_id in found_nodes:
                node_status[node_id] = NodeStatus(
                    node_id=node_id,
                    owned_files=0,
                    healthy=True,
                    repos=0
                )
        
        return node_status
    
    def get_healthy_node_count(self) -> int:
        """Get the number of healthy nodes from router logs."""
        logs = self.get_router_logs(lines=20)
        # Parse: healthy_nodes=5
        match = re.search(r'healthy_nodes=(\d+)', logs)
        if match:
            return int(match.group(1))
        return 0
    
    def wait_for_condition(self, check_fn, timeout: int = 60, interval: int = 2, desc: str = "") -> bool:
        """Wait for a condition to become true."""
        start = time.time()
        while time.time() - start < timeout:
            if check_fn():
                return True
            time.sleep(interval)
            if desc:
                elapsed = int(time.time() - start)
                self.log(f"Waiting for {desc}... ({elapsed}s)", "INFO")
        return False
    
    def wait_for_nodes_healthy(self, nodes: List[str], timeout: int = 30) -> bool:
        """Wait for specified nodes to become healthy."""
        def check():
            # First try: check healthy_nodes count for all 5 nodes
            if len(nodes) == 5:
                count = self.get_healthy_node_count()
                if count >= 5:
                    return True
            # Fallback: check individual nodes in logs
            status = self.get_node_file_counts()
            return all(n in status for n in nodes)
        return self.wait_for_condition(check, timeout, desc="nodes to be healthy")
    
    def wait_for_discovery(self, timeout: int = 30) -> bool:
        """Wait for router to discover repositories."""
        def check():
            logs = self.get_router_logs(lines=200)
            return "cluster repository discovery complete" in logs or "discovered repository" in logs
        return self.wait_for_condition(check, timeout, desc="repository discovery")
    
    def wait_for_rebalancing(self, timeout: int = 120) -> bool:
        """Wait for rebalancing to complete."""
        def check():
            logs = self.get_router_logs(lines=200)
            return "rebalancing complete, topology switched" in logs
        return self.wait_for_condition(check, timeout, desc="rebalancing completion")
    
    def wait_for_cleanup(self, timeout: int = 360) -> bool:
        """Wait for cleanup to complete (default 5 min grace + buffer)."""
        def check():
            logs = self.get_router_logs(lines=200)
            return "rebalancing cleanup complete" in logs
        return self.wait_for_condition(check, timeout, interval=10, desc="cleanup completion")
    
    def ingest_repository(self, url: str, branch: str = "master") -> bool:
        """Ingest a repository into the cluster."""
        cmd = f"./bin/monofs-admin ingest --router localhost:9090 --url {url}/tree/{branch}"
        code, stdout, stderr = self.run_command(cmd, timeout=300)
        if code != 0 or "failed" in stdout.lower():
            self.log(f"Ingest failed: {stdout} {stderr}", "FAIL")
            return False
        self.log(f"Ingested: {url}", "OK")
        return True
    
    def add_result(self, name: str, passed: bool, message: str, duration: float):
        """Add a test result."""
        self.results.append(TestResult(name, passed, message, duration))
        level = "OK" if passed else "FAIL"
        self.log(f"Test '{name}': {message} ({duration:.1f}s)", level)

    # ==================== FILESYSTEM OPERATIONS ====================
    
    def mount_filesystem(self) -> bool:
        """Mount the MonoFS FUSE filesystem."""
        # Create mount point
        if os.path.exists(self.mount_point):
            self.unmount_filesystem()
        os.makedirs(self.mount_point, exist_ok=True)
        
        # Start FUSE client in background
        cmd = f"./bin/monofs-client --router localhost:9090 --mount {self.mount_point} --debug=false"
        try:
            process = subprocess.Popen(
                cmd,
                shell=True,
                cwd=self.project_dir,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            self.fuse_pid = process.pid
            
            # Wait for mount
            time.sleep(3)
            
            # Check if mounted
            code, stdout, _ = self.run_command(f"mountpoint -q {self.mount_point} && echo mounted")
            if "mounted" in stdout:
                self.log(f"Filesystem mounted at {self.mount_point}", "FS")
                return True
            else:
                self.log("Failed to verify mount point", "FAIL")
                return False
                
        except Exception as e:
            self.log(f"Failed to mount filesystem: {e}", "FAIL")
            return False
    
    def unmount_filesystem(self) -> bool:
        """Unmount the MonoFS FUSE filesystem."""
        try:
            # Kill FUSE process if we have one
            if self.fuse_pid:
                self.run_command(f"kill {self.fuse_pid} 2>/dev/null || true")
                self.fuse_pid = None
            
            # Force unmount
            self.run_command(f"fusermount -u {self.mount_point} 2>/dev/null || true")
            self.run_command(f"umount -l {self.mount_point} 2>/dev/null || true")
            
            time.sleep(1)
            
            # Cleanup
            if os.path.exists(self.mount_point):
                try:
                    os.rmdir(self.mount_point)
                except:
                    pass
            
            self.log("Filesystem unmounted", "FS")
            return True
        except Exception as e:
            self.log(f"Error unmounting: {e}", "WARN")
            return False
    
    def get_fs_status(self) -> FSStatus:
        """Get current filesystem status."""
        status = FSStatus(
            mounted=False,
            repos_visible=0,
            files_accessible=0,
            read_errors=0,
            mount_point=self.mount_point
        )
        
        # Check if mounted
        code, stdout, _ = self.run_command(f"mountpoint -q {self.mount_point} && echo mounted")
        if "mounted" not in stdout:
            return status
        status.mounted = True
        
        # List top-level directories (repositories)
        try:
            repos = os.listdir(self.mount_point)
            status.repos_visible = len(repos)
            
            # Try to access files in each repo
            for repo in repos[:3]:  # Check first 3 repos
                repo_path = os.path.join(self.mount_point, repo)
                try:
                    for root, dirs, files in os.walk(repo_path):
                        for f in files[:10]:  # Check first 10 files per repo
                            file_path = os.path.join(root, f)
                            try:
                                with open(file_path, 'rb') as fp:
                                    content = fp.read(1024)  # Read first 1KB
                                    if len(content) > 0:
                                        status.files_accessible += 1
                            except Exception as e:
                                status.read_errors += 1
                        break  # Only check top-level files
                except Exception:
                    status.read_errors += 1
        except Exception as e:
            self.log(f"Error checking filesystem: {e}", "WARN")
        
        return status
    
    def list_fs_contents(self) -> Dict[str, List[str]]:
        """List filesystem contents by repository."""
        contents = {}
        try:
            repos = os.listdir(self.mount_point)
            for repo in repos:
                repo_path = os.path.join(self.mount_point, repo)
                if os.path.isdir(repo_path):
                    try:
                        files = []
                        for root, dirs, filenames in os.walk(repo_path):
                            rel_root = os.path.relpath(root, repo_path)
                            for f in filenames:
                                if rel_root == '.':
                                    files.append(f)
                                else:
                                    files.append(os.path.join(rel_root, f))
                            # Limit depth
                            if len(files) > 100:
                                break
                        contents[repo] = files[:100]
                    except Exception:
                        contents[repo] = []
        except Exception as e:
            self.log(f"Error listing contents: {e}", "WARN")
        return contents
    
    def read_random_files(self, count: int = 5) -> Tuple[int, int]:
        """Read random files from filesystem. Returns (success, failures)."""
        success = 0
        failures = 0
        
        try:
            repos = os.listdir(self.mount_point)
            for repo in repos:
                repo_path = os.path.join(self.mount_point, repo)
                if os.path.isdir(repo_path):
                    for root, dirs, files in os.walk(repo_path):
                        for f in files[:count]:
                            file_path = os.path.join(root, f)
                            try:
                                with open(file_path, 'rb') as fp:
                                    content = fp.read()
                                    if len(content) >= 0:  # Even empty files are OK
                                        success += 1
                            except Exception as e:
                                failures += 1
                                self.log(f"Failed to read {file_path}: {e}", "WARN")
                        break  # Only top level
                if success >= count:
                    break
        except Exception as e:
            self.log(f"Error reading files: {e}", "WARN")
        
        return success, failures

    # ==================== TEST SCENARIOS ====================
    
    def test_clean_deploy(self) -> TestResult:
        """Test 1: Clean deployment and initial setup."""
        start = time.time()
        self.log("Starting clean deployment test", "TEST")
        
        # Stop any existing containers
        self.docker_compose("down -v", timeout=60)
        time.sleep(2)
        
        # Build and start
        if not self.docker_compose("up -d --build", timeout=300):
            return TestResult("Clean Deploy", False, "Failed to start cluster", time.time() - start)
        
        # Wait for all nodes to be healthy
        time.sleep(10)  # Initial startup time
        
        if not self.wait_for_nodes_healthy(self.nodes, timeout=60):
            return TestResult("Clean Deploy", False, "Nodes did not become healthy", time.time() - start)
        
        status = self.get_node_file_counts()
        if len(status) < 5:
            return TestResult("Clean Deploy", False, f"Only {len(status)}/5 nodes healthy", time.time() - start)
        
        return TestResult("Clean Deploy", True, f"All {len(status)} nodes healthy", time.time() - start)
    
    def test_ingest_repository(self) -> TestResult:
        """Test 2: Ingest a repository."""
        start = time.time()
        self.log("Testing repository ingestion", "TEST")
        
        if not self.ingest_repository("https://github.com/go-git/go-git"):
            return TestResult("Ingest Repository", False, "Ingestion failed", time.time() - start)
        
        time.sleep(5)
        status = self.get_node_file_counts()
        
        total_files = sum(s.owned_files for s in status.values())
        if total_files == 0:
            return TestResult("Ingest Repository", False, "No files ingested", time.time() - start)
        
        return TestResult("Ingest Repository", True, f"Ingested {total_files} files across nodes", time.time() - start)
    
    def test_filesystem_mount(self) -> TestResult:
        """Test: Mount filesystem and verify access."""
        start = time.time()
        self.log("Testing filesystem mount", "TEST")
        
        if not self.mount_filesystem():
            return TestResult("FS Mount", False, "Failed to mount filesystem", time.time() - start)
        
        time.sleep(2)
        
        fs_status = self.get_fs_status()
        if not fs_status.mounted:
            return TestResult("FS Mount", False, "Filesystem not mounted", time.time() - start)
        
        if fs_status.repos_visible == 0:
            return TestResult("FS Mount", False, "No repositories visible", time.time() - start)
        
        return TestResult("FS Mount", True, 
                         f"Mounted, {fs_status.repos_visible} repos visible, {fs_status.files_accessible} files readable", 
                         time.time() - start)
    
    def test_filesystem_read(self) -> TestResult:
        """Test: Read files from mounted filesystem."""
        start = time.time()
        self.log("Testing filesystem read operations", "TEST")
        
        fs_status = self.get_fs_status()
        if not fs_status.mounted:
            return TestResult("FS Read", False, "Filesystem not mounted", time.time() - start)
        
        success, failures = self.read_random_files(count=20)
        
        if success == 0:
            return TestResult("FS Read", False, f"Could not read any files ({failures} failures)", time.time() - start)
        
        if failures > success:
            return TestResult("FS Read", False, 
                            f"Too many read failures: {success} success, {failures} failures", 
                            time.time() - start)
        
        return TestResult("FS Read", True, 
                         f"Read {success} files successfully, {failures} failures", 
                         time.time() - start)
    
    def test_fs_during_node_failure(self, node: str) -> TestResult:
        """Test: Filesystem remains accessible during node failure."""
        start = time.time()
        self.log(f"Testing filesystem during {node} failure", "TEST")
        
        # Initial read
        success_before, failures_before = self.read_random_files(count=10)
        self.log(f"Before failure: {success_before} reads OK", "FS")
        
        # Stop the node
        self.log(f"Stopping {node}...", "INFO")
        if not self.docker_compose(f"stop {node}"):
            return TestResult(f"FS During {node} Failure", False, "Failed to stop node", time.time() - start)
        
        time.sleep(5)
        
        # Read during failure
        success_during, failures_during = self.read_random_files(count=10)
        self.log(f"During failure: {success_during} reads OK, {failures_during} failures", "FS")
        
        # Start the node back
        self.log(f"Starting {node}...", "INFO")
        self.docker_compose(f"start {node}")
        time.sleep(10)
        
        # Read after recovery
        success_after, failures_after = self.read_random_files(count=10)
        self.log(f"After recovery: {success_after} reads OK", "FS")
        
        # Filesystem should remain accessible (HRW routes to other nodes)
        if success_during == 0:
            return TestResult(f"FS During {node} Failure", False, 
                            "No reads succeeded during failure", time.time() - start)
        
        return TestResult(f"FS During {node} Failure", True, 
                         f"Reads: before={success_before}, during={success_during}, after={success_after}", 
                         time.time() - start)
    
    def test_single_node_failure(self, node: str) -> TestResult:
        """Test: Single node failure and recovery."""
        start = time.time()
        self.log(f"Testing {node} failure and recovery", "TEST")
        
        # Get initial state
        initial_status = self.get_node_file_counts()
        if node not in initial_status:
            return TestResult(f"{node} Failover", False, f"{node} not in initial status", time.time() - start)
        
        initial_files = initial_status[node].owned_files
        self.log(f"{node} initial files: {initial_files}", "INFO")
        
        # Stop the node
        self.log(f"Stopping {node}...", "INFO")
        if not self.docker_compose(f"stop {node}"):
            return TestResult(f"{node} Failover", False, "Failed to stop node", time.time() - start)
        
        time.sleep(10)  # Wait for router to detect failure
        
        # Start the node again
        self.log(f"Starting {node}...", "INFO")
        if not self.docker_compose(f"start {node}"):
            return TestResult(f"{node} Failover", False, "Failed to start node", time.time() - start)
        
        # Wait for node to recover
        time.sleep(15)
        
        if not self.wait_for_nodes_healthy([node], timeout=60):
            return TestResult(f"{node} Failover", False, "Node did not recover", time.time() - start)
        
        # Check if rebalancing was triggered
        if not self.wait_for_rebalancing(timeout=120):
            self.log("Rebalancing not detected in logs, continuing...", "WARN")
        
        # Get final status
        final_status = self.get_node_file_counts()
        if node not in final_status:
            return TestResult(f"{node} Failover", False, "Node not in final status", time.time() - start)
        
        final_files = final_status[node].owned_files
        self.log(f"{node} final files: {final_files}", "INFO")
        
        # Node should have files after recovery
        if final_files == 0 and initial_files > 0:
            return TestResult(f"{node} Failover", False, "Node has 0 files after recovery", time.time() - start)
        
        return TestResult(f"{node} Failover", True, 
                         f"Recovered: {initial_files} -> {final_files} files", time.time() - start)
    
    def test_router_restart_discovery(self) -> TestResult:
        """Test: Router restart should discover existing repositories."""
        start = time.time()
        self.log("Testing router restart discovery", "TEST")
        
        # Get initial cluster state
        initial_status = self.get_node_file_counts()
        total_files_before = sum(s.owned_files for s in initial_status.values())
        
        # Restart routers
        self.log("Restarting routers...", "INFO")
        if not self.docker_compose("restart router1 router2"):
            return TestResult("Router Discovery", False, "Failed to restart routers", time.time() - start)
        
        time.sleep(10)  # Wait for startup
        
        # Wait for discovery
        if not self.wait_for_discovery(timeout=30):
            return TestResult("Router Discovery", False, "Discovery not detected", time.time() - start)
        
        # Check logs for discovery
        logs = self.get_router_logs(lines=300)
        if "discovered repository" not in logs and "cluster repository discovery complete" not in logs:
            return TestResult("Router Discovery", False, "No discovery logs found", time.time() - start)
        
        # Verify cluster state is preserved
        time.sleep(5)
        final_status = self.get_node_file_counts()
        total_files_after = sum(s.owned_files for s in final_status.values())
        
        # Allow some variance due to timing
        if abs(total_files_after - total_files_before) > total_files_before * 0.1:
            return TestResult("Router Discovery", False, 
                            f"File count changed significantly: {total_files_before} -> {total_files_after}", 
                            time.time() - start)
        
        return TestResult("Router Discovery", True, 
                         f"Discovered repos, files preserved: {total_files_after}", time.time() - start)
    
    def test_fs_after_router_restart(self) -> TestResult:
        """Test: Filesystem accessible after router restart."""
        start = time.time()
        self.log("Testing filesystem after router restart", "TEST")
        
        # Check current FS status
        fs_status = self.get_fs_status()
        if not fs_status.mounted:
            # Try to remount
            self.unmount_filesystem()
            if not self.mount_filesystem():
                return TestResult("FS After Router Restart", False, 
                                "Failed to remount filesystem", time.time() - start)
        
        time.sleep(2)
        
        success, failures = self.read_random_files(count=10)
        
        if success == 0:
            return TestResult("FS After Router Restart", False, 
                            "No files readable after restart", time.time() - start)
        
        return TestResult("FS After Router Restart", True, 
                         f"FS accessible: {success} files read, {failures} failures", 
                         time.time() - start)
    
    def test_node_offline_during_ingest(self, node: str) -> TestResult:
        """Test: Node offline during ingestion should get files on recovery."""
        start = time.time()
        self.log(f"Testing {node} offline during ingestion", "TEST")
        
        # Stop the node
        self.log(f"Stopping {node}...", "INFO")
        if not self.docker_compose(f"stop {node}"):
            return TestResult(f"{node} Offline Ingest", False, "Failed to stop node", time.time() - start)
        
        time.sleep(5)
        
        # Ingest a new repository while node is offline
        self.log("Ingesting repository while node is offline...", "INFO")
        if not self.ingest_repository("https://github.com/charmbracelet/bubbletea", branch="main"):
            # Start node back before returning
            self.docker_compose(f"start {node}")
            return TestResult(f"{node} Offline Ingest", False, "Ingestion failed", time.time() - start)
        
        time.sleep(5)
        
        # Start the node
        self.log(f"Starting {node}...", "INFO")
        if not self.docker_compose(f"start {node}"):
            return TestResult(f"{node} Offline Ingest", False, "Failed to start node", time.time() - start)
        
        # Wait for recovery and rebalancing
        time.sleep(15)
        
        if not self.wait_for_nodes_healthy([node], timeout=60):
            return TestResult(f"{node} Offline Ingest", False, "Node did not recover", time.time() - start)
        
        # Wait for rebalancing
        self.wait_for_rebalancing(timeout=120)
        
        # Get final status
        time.sleep(5)
        final_status = self.get_node_file_counts()
        
        if node not in final_status:
            return TestResult(f"{node} Offline Ingest", False, "Node not in final status", time.time() - start)
        
        final_files = final_status[node].owned_files
        
        # Node should have received files from rebalancing
        if final_files == 0:
            return TestResult(f"{node} Offline Ingest", False, 
                            "Node has 0 files after recovery (expected rebalancing)", time.time() - start)
        
        return TestResult(f"{node} Offline Ingest", True, 
                         f"Node recovered with {final_files} files", time.time() - start)
    
    def test_fs_new_repo_visible(self) -> TestResult:
        """Test: Newly ingested repository is visible in filesystem."""
        start = time.time()
        self.log("Testing new repository visibility in filesystem", "TEST")
        
        # Remount to pick up new repos
        self.unmount_filesystem()
        time.sleep(2)
        if not self.mount_filesystem():
            return TestResult("FS New Repo", False, "Failed to remount", time.time() - start)
        
        time.sleep(3)
        
        # Check contents
        contents = self.list_fs_contents()
        
        if len(contents) == 0:
            return TestResult("FS New Repo", False, "No repositories in filesystem", time.time() - start)
        
        # List repos and file counts
        repo_summary = ", ".join(f"{repo}:{len(files)}" for repo, files in contents.items())
        
        return TestResult("FS New Repo", True, 
                         f"Visible repos: {repo_summary}", time.time() - start)
    
    def test_multiple_node_failure(self) -> TestResult:
        """Test: Multiple nodes fail and recover."""
        start = time.time()
        failed_nodes = ["node2", "node4"]
        self.log(f"Testing multiple node failure: {failed_nodes}", "TEST")
        
        # Get initial state
        initial_status = self.get_node_file_counts()
        
        # Stop multiple nodes
        for node in failed_nodes:
            self.log(f"Stopping {node}...", "INFO")
            self.docker_compose(f"stop {node}")
        
        time.sleep(10)
        
        # Verify remaining nodes are still working
        status_partial = self.get_node_file_counts()
        remaining_nodes = [n for n in self.nodes if n not in failed_nodes]
        
        if not all(n in status_partial for n in remaining_nodes):
            # Start nodes back
            for node in failed_nodes:
                self.docker_compose(f"start {node}")
            return TestResult("Multi-Node Failure", False, 
                            "Remaining nodes not healthy", time.time() - start)
        
        # Start nodes back
        for node in failed_nodes:
            self.log(f"Starting {node}...", "INFO")
            self.docker_compose(f"start {node}")
        
        time.sleep(15)
        
        # Wait for all nodes to be healthy
        if not self.wait_for_nodes_healthy(failed_nodes, timeout=90):
            return TestResult("Multi-Node Failure", False, 
                            "Not all nodes recovered", time.time() - start)
        
        # Wait for rebalancing
        self.wait_for_rebalancing(timeout=120)
        
        # Get final status
        time.sleep(5)
        final_status = self.get_node_file_counts()
        
        all_recovered = all(n in final_status for n in failed_nodes)
        if not all_recovered:
            return TestResult("Multi-Node Failure", False, 
                            "Not all nodes in final status", time.time() - start)
        
        recovered_files = sum(final_status[n].owned_files for n in failed_nodes)
        
        return TestResult("Multi-Node Failure", True, 
                         f"All nodes recovered, {recovered_files} total files on recovered nodes", 
                         time.time() - start)
    
    def test_fs_during_multi_node_failure(self) -> TestResult:
        """Test: Filesystem accessible during multi-node failure."""
        start = time.time()
        failed_nodes = ["node1", "node3"]
        self.log(f"Testing FS during multi-node failure: {failed_nodes}", "TEST")
        
        # Initial read
        success_before, _ = self.read_random_files(count=10)
        
        # Stop nodes
        for node in failed_nodes:
            self.docker_compose(f"stop {node}")
        time.sleep(5)
        
        # Read during failure
        success_during, failures_during = self.read_random_files(count=10)
        
        # Start nodes back
        for node in failed_nodes:
            self.docker_compose(f"start {node}")
        time.sleep(10)
        
        # Read after
        success_after, _ = self.read_random_files(count=10)
        
        # FS should remain accessible with 3/5 nodes
        if success_during == 0:
            return TestResult("FS Multi-Node Failure", False, 
                            "No reads during failure", time.time() - start)
        
        return TestResult("FS Multi-Node Failure", True, 
                         f"Reads: before={success_before}, during={success_during}, after={success_after}", 
                         time.time() - start)
    
    def test_cleanup_after_rebalancing(self) -> TestResult:
        """Test: Old files are cleaned up after rebalancing."""
        start = time.time()
        self.log("Testing cleanup after rebalancing", "TEST")
        
        # Check if cleanup already happened
        logs = self.get_router_logs(lines=500)
        
        if "rebalancing cleanup complete" in logs:
            # Extract cleanup stats
            match = re.search(r'files_deleted=(\d+).*files_failed=(\d+)', logs)
            if match:
                deleted = int(match.group(1))
                failed = int(match.group(2))
                return TestResult("Cleanup", True, 
                                f"Cleanup completed: {deleted} deleted, {failed} failed", 
                                time.time() - start)
        
        # If not, wait for it
        self.log("Waiting for cleanup (5 minute grace period)...", "INFO")
        if not self.wait_for_cleanup(timeout=400):
            return TestResult("Cleanup", False, "Cleanup did not complete", time.time() - start)
        
        # Check cleanup results
        logs = self.get_router_logs(lines=200)
        match = re.search(r'files_deleted=(\d+).*files_failed=(\d+)', logs)
        if match:
            deleted = int(match.group(1))
            failed = int(match.group(2))
            return TestResult("Cleanup", True, 
                            f"Cleanup completed: {deleted} deleted, {failed} failed", 
                            time.time() - start)
        
        return TestResult("Cleanup", False, "Could not parse cleanup results", time.time() - start)
    
    def test_file_distribution_balance(self) -> TestResult:
        """Test: Files should be roughly balanced across nodes."""
        start = time.time()
        self.log("Testing file distribution balance", "TEST")
        
        status = self.get_node_file_counts()
        
        if len(status) < 5:
            return TestResult("Balance Check", False, 
                            f"Only {len(status)}/5 nodes reporting", time.time() - start)
        
        file_counts = [s.owned_files for s in status.values()]
        total_files = sum(file_counts)
        avg_files = total_files / len(file_counts)
        
        # Calculate deviation
        max_deviation = max(abs(f - avg_files) / avg_files for f in file_counts) if avg_files > 0 else 0
        
        # Files should be within 80% of average (HRW + multiple failovers causes variance)
        if max_deviation > 0.8:
            details = ", ".join(f"{s.node_id}:{s.owned_files}" for s in status.values())
            return TestResult("Balance Check", False, 
                            f"Imbalanced distribution ({max_deviation:.0%} deviation): {details}", 
                            time.time() - start)
        
        details = ", ".join(f"{s.node_id}:{s.owned_files}" for s in status.values())
        return TestResult("Balance Check", True, 
                         f"Balanced ({max_deviation:.0%} max deviation): {details}", 
                         time.time() - start)

    def run_all_tests(self, quick: bool = False, skip_fs: bool = False):
        """Run the complete test suite."""
        self.log("=" * 60, "INFO")
        self.log("MonoFS Failover & Rebalancing Test Suite", "INFO")
        self.log("=" * 60, "INFO")
        
        suite_start = time.time()
        
        try:
            # Phase 1: Setup
            self.log("\n📦 PHASE 1: Setup", "TEST")
            result = self.test_clean_deploy()
            self.add_result(result.name, result.passed, result.message, result.duration)
            if not result.passed:
                self.print_summary()
                return
            
            result = self.test_ingest_repository()
            self.add_result(result.name, result.passed, result.message, result.duration)
            if not result.passed:
                self.print_summary()
                return
            
            # Phase 2: Filesystem Tests
            if not skip_fs:
                self.log("\n📂 PHASE 2: Filesystem Tests", "TEST")
                result = self.test_filesystem_mount()
                self.add_result(result.name, result.passed, result.message, result.duration)
                
                if result.passed:
                    result = self.test_filesystem_read()
                    self.add_result(result.name, result.passed, result.message, result.duration)
            
            # Phase 3: Single Node Failures
            self.log("\n🔥 PHASE 3: Single Node Failure Tests", "TEST")
            for node in ["node1", "node3", "node5"]:
                result = self.test_single_node_failure(node)
                self.add_result(result.name, result.passed, result.message, result.duration)
                
                if not skip_fs:
                    result = self.test_fs_during_node_failure(node)
                    self.add_result(result.name, result.passed, result.message, result.duration)
                
                time.sleep(5)  # Brief pause between tests
            
            # Phase 4: Router Restart
            self.log("\n🔄 PHASE 4: Router Discovery Test", "TEST")
            result = self.test_router_restart_discovery()
            self.add_result(result.name, result.passed, result.message, result.duration)
            
            if not skip_fs:
                result = self.test_fs_after_router_restart()
                self.add_result(result.name, result.passed, result.message, result.duration)
            
            # Phase 5: Node Offline During Ingest
            self.log("\n📥 PHASE 5: Node Offline During Ingest", "TEST")
            result = self.test_node_offline_during_ingest("node2")
            self.add_result(result.name, result.passed, result.message, result.duration)
            
            if not skip_fs:
                result = self.test_fs_new_repo_visible()
                self.add_result(result.name, result.passed, result.message, result.duration)
            
            # Phase 6: Multiple Node Failure
            self.log("\n💥 PHASE 6: Multiple Node Failure", "TEST")
            result = self.test_multiple_node_failure()
            self.add_result(result.name, result.passed, result.message, result.duration)
            
            if not skip_fs:
                result = self.test_fs_during_multi_node_failure()
                self.add_result(result.name, result.passed, result.message, result.duration)
            
            # Phase 7: Balance Check
            self.log("\n⚖️ PHASE 7: Distribution Balance", "TEST")
            result = self.test_file_distribution_balance()
            self.add_result(result.name, result.passed, result.message, result.duration)
            
            # Phase 8: Cleanup (skip if quick mode)
            if not quick:
                self.log("\n🧹 PHASE 8: Cleanup Verification", "TEST")
                result = self.test_cleanup_after_rebalancing()
                self.add_result(result.name, result.passed, result.message, result.duration)
            else:
                self.log("\n⏭️ Skipping cleanup test (quick mode)", "INFO")
            
        finally:
            # Cleanup
            if not skip_fs:
                self.unmount_filesystem()
        
        total_duration = time.time() - suite_start
        self.print_summary(total_duration)
    
    def print_summary(self, total_duration: float = 0):
        """Print test results summary."""
        self.log("\n" + "=" * 60, "INFO")
        self.log("TEST RESULTS SUMMARY", "INFO")
        self.log("=" * 60, "INFO")
        
        passed = sum(1 for r in self.results if r.passed)
        failed = sum(1 for r in self.results if not r.passed)
        
        for result in self.results:
            status = "✅ PASS" if result.passed else "❌ FAIL"
            self.log(f"{status} | {result.name}: {result.message} ({result.duration:.1f}s)", 
                    "OK" if result.passed else "FAIL")
        
        self.log("-" * 60, "INFO")
        self.log(f"Total: {passed} passed, {failed} failed out of {len(self.results)} tests", 
                "OK" if failed == 0 else "FAIL")
        
        if total_duration > 0:
            self.log(f"Total duration: {total_duration:.1f}s ({total_duration/60:.1f} minutes)", "INFO")
        
        # Exit with appropriate code
        if failed > 0:
            sys.exit(1)


def main():
    quick_mode = "--quick" in sys.argv
    skip_fs = "--skip-fs" in sys.argv
    
    suite = MonoFSTestSuite()
    
    try:
        suite.run_all_tests(quick=quick_mode, skip_fs=skip_fs)
    except KeyboardInterrupt:
        suite.log("\n\nTest suite interrupted by user", "WARN")
        suite.unmount_filesystem()
        suite.print_summary()
        sys.exit(130)
    except Exception as e:
        suite.log(f"\n\nTest suite failed with exception: {e}", "FAIL")
        suite.unmount_filesystem()
        import traceback
        traceback.print_exc()
        sys.exit(1)


if __name__ == "__main__":
    main()

