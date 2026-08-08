Generate the document body for the next immutable artifact as one JSON object.
Return only the document body. Do not wrap it in artifact fields such as kind, version, or document_json.
Replace the placeholder values in the required JSON shape below and preserve every key:
{{ .Contract }}

Nasuta renders Markdown chapters deterministically from these keys. Do not rename, merge, omit, or add fields.
Write every natural-language field in Simplified Chinese. Keep only code identifiers, commands, file paths, API or schema names, and untranslatable proper nouns in their original form.

Target artifact kind: {{ .Kind }}

Input:
{{ .Input }}
