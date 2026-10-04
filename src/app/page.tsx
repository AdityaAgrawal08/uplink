export default function Home() {
  return (
    <div className="container">
      <h1>R2-Uplink File-Sharing Platform</h1>
      <p>This is a CLI-only file-sharing service. Browser-based uploads are not supported.</p>
      <h2>Installation</h2>
      <pre className="code-block">
        curl -sSf https://raw.githubusercontent.com/AdityaAgrawal08/uplink/main/install.sh | sh
      </pre>
      <h2>Usage</h2>
      <p>To share a file or directory:</p>
      <pre className="code-block">
        uplink send &lt;filepath-or-directory&gt;
      </pre>
    </div>
  );
}
