import 'package:flutter/material.dart';

import '../theme/tokens.dart';

class SettingsPage extends StatelessWidget {
  final TextEditingController addrController;
  final TextEditingController tokenController;
  final VoidCallback onApply;

  const SettingsPage({
    super.key,
    required this.addrController,
    required this.tokenController,
    required this.onApply,
  });

  @override
  Widget build(BuildContext context) {
    return ListView(
      padding: const EdgeInsets.all(32),
      children: [
        Container(
          padding: const EdgeInsets.all(32),
          decoration: BoxDecoration(
            color: Tokens.cardBg,
            borderRadius: BorderRadius.circular(Tokens.cardRadius),
          ),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text('CONNECTION', style: Theme.of(context).textTheme.labelSmall),
              const SizedBox(height: 16),
              TextField(
                controller: addrController,
                decoration: const InputDecoration(
                  labelText: 'Admin address',
                  hintText: 'http://127.0.0.1:18099',
                  border: OutlineInputBorder(),
                ),
              ),
              const SizedBox(height: 16),
              TextField(
                controller: tokenController,
                obscureText: true,
                decoration: const InputDecoration(
                  labelText: 'Token',
                  border: OutlineInputBorder(),
                ),
              ),
              const SizedBox(height: 24),
              PillButton('Apply', onPressed: onApply),
            ],
          ),
        ),
      ],
    );
  }
}
