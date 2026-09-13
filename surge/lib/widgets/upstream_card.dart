import 'package:flutter/material.dart';

import '../api/client.dart';
import '../theme/tokens.dart';

class StatusDot extends StatelessWidget {
  final String status;
  const StatusDot(this.status, {super.key});

  @override
  Widget build(BuildContext context) {
    final color = switch (status) {
      'enabled' => Tokens.statusDot,
      'half_open' => Tokens.warn,
      'auto_disabled' => Tokens.err,
      _ => Tokens.textMuted,
    };
    return Container(
      width: 10,
      height: 10,
      decoration: BoxDecoration(color: color, shape: BoxShape.circle),
    );
  }
}

// UpstreamCard follows KiroaaS UsageCard: balance progress bar + collapsible
// detail + manual controls.
class UpstreamCard extends StatefulWidget {
  final Upstream upstream;
  final double? maxBalance;
  final Future<void> Function(bool enabled) onToggle;
  final Future<List<dynamic>> Function() onProbe;

  const UpstreamCard({
    super.key,
    required this.upstream,
    this.maxBalance,
    required this.onToggle,
    required this.onProbe,
  });

  @override
  State<UpstreamCard> createState() => _UpstreamCardState();
}

class _UpstreamCardState extends State<UpstreamCard> {
  bool expanded = false;
  bool busy = false;

  @override
  Widget build(BuildContext context) {
    final u = widget.upstream;
    final running = u.status == 'enabled';
    return AnimatedContainer(
      duration: const Duration(milliseconds: 250),
      decoration: BoxDecoration(
        color: running ? Tokens.accent : Tokens.cardBg,
        borderRadius: BorderRadius.circular(Tokens.cardRadius),
      ),
      padding: const EdgeInsets.all(24),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            children: [
              StatusDot(u.status),
              const SizedBox(width: 10),
              Expanded(
                child: Text(u.name,
                    style: const TextStyle(fontSize: 18, fontWeight: FontWeight.bold)),
              ),
              Text(u.status, style: const TextStyle(color: Tokens.textMuted, fontSize: 12)),
            ],
          ),
          const SizedBox(height: 8),
          Text('${u.provider} · ${u.protocol} · ${u.models.join(", ")}',
              style: const TextStyle(color: Tokens.textMuted, fontSize: 12),
              overflow: TextOverflow.ellipsis),
          if (u.lastBalance != null) ...[
            const SizedBox(height: 12),
            Text('\$${u.lastBalance!.toStringAsFixed(2)}',
                style: Theme.of(context).textTheme.headlineSmall),
            if (widget.maxBalance != null && widget.maxBalance! > 0) ...[
              const SizedBox(height: 8),
              ClipRRect(
                borderRadius: BorderRadius.circular(4),
                child: LinearProgressIndicator(
                  value: (u.lastBalance! / widget.maxBalance!).clamp(0.0, 1.0),
                  minHeight: 6,
                  backgroundColor: Tokens.contentBg,
                  valueColor: AlwaysStoppedAnimation(
                      running ? Tokens.accentText : Tokens.statusDot),
                ),
              ),
            ],
          ],
          if (u.reason.isNotEmpty) ...[
            const SizedBox(height: 8),
            Container(
              padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 6),
              decoration: BoxDecoration(
                color: Tokens.errBg,
                borderRadius: BorderRadius.circular(Tokens.iconRadius),
              ),
              child: Text(u.reason, style: const TextStyle(color: Tokens.err, fontSize: 12)),
            ),
          ],
          const SizedBox(height: 12),
          Row(
            children: [
              TextButton(
                onPressed: busy
                    ? null
                    : () async {
                        setState(() => busy = true);
                        try {
                          await widget.onToggle(!running);
                        } finally {
                          if (mounted) setState(() => busy = false);
                        }
                      },
                child: Text(running ? 'Disable' : 'Enable'),
              ),
              TextButton(
                onPressed: busy
                    ? null
                    : () async {
                        setState(() => busy = true);
                        try {
                          await widget.onProbe();
                        } finally {
                          if (mounted) setState(() => busy = false);
                        }
                      },
                child: const Text('Probe now'),
              ),
              IconButton(
                icon: Icon(expanded ? Icons.expand_less : Icons.expand_more),
                onPressed: () => setState(() => expanded = !expanded),
              ),
            ],
          ),
          if (expanded)
            Padding(
              padding: const EdgeInsets.only(top: 8),
              child: Text(
                'base_url: ${u.baseUrl}\nconsecutive failures: ${u.consecFail}\nbalance at: ${u.lastBalanceAt ?? "-"}\nrequests: ${u.requests} (ok ${u.successes})  last latency: ${u.lastLatencyMs}ms',
                style: const TextStyle(fontFamily: 'monospace', fontSize: 12),
              ),
            ),
        ],
      ),
    );
  }
}
