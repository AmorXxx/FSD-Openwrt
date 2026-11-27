FROM python:3.12-slim
RUN pip install --no-cache-dir PyMySQL==1.2.4
WORKDIR /app
COPY server.py /app/server.py
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
CMD ["python", "/app/server.py"]
