// Unit tests for the k8e-sandbox client transport helpers: language command
// wrapping (mirrors pkg/sandboxcli buildCommand) and profile resolution
// (mirrors pkg/sandboxcli/profile.go KIP-17).
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { buildSandboxCommand } from '@k8e-sandbox/dsh-k8e-sandbox-client/grpc'
import { resolveSandboxTransport } from '@k8e-sandbox/dsh-k8e-sandbox-client'

// ---- buildSandboxCommand (language wrapping) ---------------------------
{
  // bash passes through
  assert.equal(buildSandboxCommand('bash', 'echo hi'), 'echo hi')
  assert.equal(buildSandboxCommand(undefined, 'stat -c %s /x'), 'stat -c %s /x')

  // python single-line wraps with -c; multi-line uses the temp file
  assert.equal(buildSandboxCommand('python', 'print(1)'), 'python3 -c "print(1)"')
  assert.equal(buildSandboxCommand('python', 'a = 1\nprint(a)'), 'python3 /workspace/_k8e_run.py')

  // node single-line uses -e; multi-line uses the temp file
  assert.equal(buildSandboxCommand('node', 'console.log(1)'), 'node -e "console.log(1)"')
  assert.equal(buildSandboxCommand('js', 'const a = 1\nconsole.log(a)'), 'node /workspace/_k8e_run.js')

  // ts always uses the temp file with TMPDIR redirect
  assert.equal(buildSandboxCommand('ts', 'const x = 1'), 'TMPDIR=/workspace tsx /workspace/_k8e_run.ts')

  // embedded quotes survive the JSON stringify wrapper
  assert.equal(buildSandboxCommand('python', `print('a"b')`), 'python3 -c "print(\'a\\\"b\')"')
}

// ---- resolveSandboxTransport (profiles.yaml KIP-17) --------------------
{
  const dir = mkdtempSync(join(tmpdir(), 'k8e-profiles-'))
  const profilesPath = join(dir, 'profiles.yaml')
  try {
    writeFileSync(profilesPath, [
      'version: 1',
      'current_profile: default',
      'profiles:',
      '  default:',
      '    endpoint: ec2-3-37-16-143.ap-northeast-2.compute.amazonaws.com:50051',
      '    cert_dir: ""',
      '    device_name: ""',
      '  local:',
      '    endpoint: 127.0.0.1:50051',
      '    cert_dir: /tmp/certs',
      '',
    ].join('\n'), 'utf8')

    const prevConfig = process.env.K8E_SANDBOX_CONFIG
    const prevCertDir = process.env.K8E_SANDBOX_CERT_DIR
    const prevEndpoint = process.env.K8E_SANDBOX_ENDPOINT
    const prevProfile = process.env.K8E_SANDBOX_PROFILE
    try {
      delete process.env.K8E_SANDBOX_ENDPOINT
      delete process.env.K8E_SANDBOX_PROFILE
      process.env.K8E_SANDBOX_CERT_DIR = dir
      process.env.K8E_SANDBOX_CONFIG = profilesPath

      // current_profile default wins (auto-discovery source recorded)
      const viaDefault = resolveSandboxTransport()
      assert.deepEqual(viaDefault, {
        endpoint: 'ec2-3-37-16-143.ap-northeast-2.compute.amazonaws.com:50051',
        source: 'profile',
        profile: 'default',
      })

      // explicit profile wins
      const viaProfile = resolveSandboxTransport({ profile: 'local' })
      assert.deepEqual(viaProfile, {
        endpoint: '127.0.0.1:50051',
        certDir: '/tmp/certs',
        source: 'profile',
        profile: 'local',
      })

      // explicit endpoint beats profile entirely (env cert dir still honored)
      const viaExplicit = resolveSandboxTransport({ endpoint: 'gw.example.com:50051', profile: 'local' })
      assert.deepEqual(viaExplicit, {
        endpoint: 'gw.example.com:50051',
        certDir: dir,
        source: 'config',
      })
    } finally {
      if (prevConfig !== undefined) process.env.K8E_SANDBOX_CONFIG = prevConfig
      else delete process.env.K8E_SANDBOX_CONFIG
      if (prevCertDir !== undefined) process.env.K8E_SANDBOX_CERT_DIR = prevCertDir
      else delete process.env.K8E_SANDBOX_CERT_DIR
      if (prevEndpoint !== undefined) process.env.K8E_SANDBOX_ENDPOINT = prevEndpoint
      else delete process.env.K8E_SANDBOX_ENDPOINT
      if (prevProfile !== undefined) process.env.K8E_SANDBOX_PROFILE = prevProfile
      else delete process.env.K8E_SANDBOX_PROFILE
    }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

console.log('✔ client transport helpers test passed (buildSandboxCommand, resolveSandboxTransport)')
