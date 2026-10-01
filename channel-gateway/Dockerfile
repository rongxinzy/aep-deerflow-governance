# de-channel-gateway — the always-on platform websocket leg for IM channels.
# Built FROM the runtime image so the gateway imports the exact same channel
# code the runtimes use (protocol handling never drifts between the two).
FROM 172.18.5.251:8081/zhiyuan/deerflow-gov:v18
WORKDIR /app/backend
COPY gateway_app.py /gateway/gateway_app.py
ENV PYTHONPATH=/opt/governance:.
EXPOSE 8899
ENTRYPOINT ["/app/backend/.venv/bin/python", "/gateway/gateway_app.py"]
