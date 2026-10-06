import type { Metadata } from "next";
import CodeGraph from "@/components/CodeGraph";

export const metadata: Metadata = {
  title: "Code Graph | R2-Uplink",
  description:
    "Interactive dependency graph of the Go CLI and Next.js web sources (public/graph.json).",
};

export default function GraphPage() {
  return <CodeGraph />;
}
